# v2 实现决策与 hesitation 记录

> 实现 `DESIGN-v2-dynamic-mode.md` 期间,凡是设计稿没写死、写得自相矛盾、或实测后我认为应当偏离的地方,
> 都记在这里:**我当时犹豫什么 → 我查到了什么 → 我最后怎么定的 → 如果定错了代价是什么**。
>
> 标记含义:
> - `[遵循]` 按设计稿实现,只是补齐了设计稿没写的细节
> - `[偏离]` 有意不按设计稿做,附理由
> - `[补洞]` 设计稿缺失或自相矛盾,我替它做了决定

---

## D1 `[偏离]` A2A 内部路由不走 loopback HTTP,台账即协议事实源

**犹豫**:设计稿 §3 决策 1 说「A2A 为 agent 间唯一协议」,§9 的 P2 说「内部路由切到 A2A client」。
照做的话,coordinator 委派一个任务要走:MCP 工具 → hub 内 A2A client → localhost HTTP → hub 内 A2A server → 台账。
同一个进程里绕一圈 HTTP。

**查到**:
- `a2aclient` 包会拉进 `google.golang.org/grpc` + `protobuf` 整条依赖链;只用 `a2a` + `a2asrv` 则仅新增
  `google/uuid` 与 `golang.org/x/sync` 两个间接依赖(实测 `go mod tidy` 对比)。对一个以「唯一第三方依赖」
  为卖点的项目,这个差别不小。
- `a2asrv.RequestHandler` 是个 10 方法接口,可以自己实现并直接以台账为后端;不必被 SDK 自带的
  `defaultRequestHandler` + 内置 task store 绑架(那会造成台账与 SDK store 两份状态)。

**决定**:
- 台账(`internal/hub/ledger.go`)是**唯一**事实源,其状态机直接采用 A2A 的 `TaskState`。
- `a2asrv.NewJSONRPCHandler` 挂在 `/a2a/agents/{name}`,后面接我自己实现的 `RequestHandler`,读写同一个台账。
  于是**内部委派产生的任务和外部 A2A client 创建的任务,在 `tasks/get` / `tasks/list` 里完全等价可见**。
- 内部委派直接调台账 API,不自打 HTTP。

**代价**:严格意义上,进程内那一跳没有「过协议」。但协议要买的两样东西——外部可互通的门面、台账无盲区——
都拿到了;付出的只是「形式上的自我调用」。如果将来要做跨机器分布式(设计稿的非目标),
把台账调用换成 `a2aclient` 是一处局部替换,因为两侧共用同一组 A2A 类型。

---

## D2 `[偏离]` 成本核算:用 sessionId glob 找 transcript,不复刻路径编码规则

**犹豫**:设计稿 §5.5 说 transcript 在 `~/.claude/projects/<agent-home 编码路径>/<sessionId>.jsonl`,
要按 agent home 的绝对路径反推出目录名。风险条目 4 也点了这个依赖。

**查到**:实测编码规则是 `/` → `-`,但路径段本身以 `-` 开头会产生 `--`,对 `.`/`_` 的处理还得再试。
反推规则脆且没必要——**文件名就是 sessionId,而 sessionId 是 ACP `session/new` 直接返回给我们的**。

**决定**:`filepath.Glob(~/.claude/projects/*/<sessionID>.jsonl)`。不关心目录名怎么编码。

**代价**:理论上 sessionId 撞名会拿错文件(UUID,可忽略);多一次目录扫描(每任务一次,可忽略)。
换来的是对 CLI 路径编码规则升级的完全免疫。

---

## D3 `[补洞]` `propose_plan` 工具:设计稿自相矛盾,补进 coordinator 工具集

**犹豫**:§5.4 审批点写「coordinator 首次 delegate 前必须先调 `propose_plan`」,
但 §5.1 的 coordinator 工具表里**根本没有这个工具**。

**决定**:补进工具集。语义:提交任务清单 + 新 agent 提案 → run 转 `awaiting_approval` → 阻塞在工具调用里
等 UI 批准/拒绝 → 批准则工具返回 ok 并解锁 delegate;拒绝则返回错误且 run 取消。
`ApprovalPolicy=initial` 时,delegate 在 propose_plan 通过前一律返回结构化错误(硬拦,不靠自觉)。

---

## D4 `[补洞]` `send_message` 打给 working 任务:落到下一个回合边界

**犹豫**:worker 处理任务时是**卡在 ACP `session/prompt` 里面**的。ACP 没有「给进行中的回合插一句话」这种原语。
那 coordinator 对一个 `working` 任务调 `send_message` 到底发生什么?设计稿没说。

**决定**:分两种情况,语义不同但都诚实:
- 任务是 `input-required`(worker 卡在 `ask_coordinator` 工具调用里)→ 消息**就是**那次工具调用的返回值,
  worker 原地继续,不消耗新回合。
- 任务是 `working` → 消息入队,当前回合结束后作为**追加的一轮 `session/prompt`** 投递(计入 `MaxTurnsPerTask`)。
  这一轮开始前,上一轮的信封若已产出会被作废——因为任务并没有结束。

工具返回值里明确告诉 coordinator 是哪一种(`"delivery": "immediate" | "queued_next_turn"`),
让它自己判断要不要等。

**代价**:对 `working` 任务的 steering 有延迟,最坏情况是「worker 已经写完信封了才收到别干了」。
但替代方案(强杀会话重发)会丢掉整轮上下文,更糟。

---

## D5 `[补洞]` `await` 有硬上限,退化为「阻塞式轮询」

**犹豫**:`await` 是个可能挂几十分钟的 MCP 工具调用。Claude Code 侧对 MCP 工具有自己的超时
(`MCP_TOOL_TIMEOUT`,不由我们控制),挂太久会被客户端单方面判死,coordinator 拿到的是一个"工具失败",
而不是"任务还在跑",这会诱导它做出错误决策。

**决定**:`await` 实际阻塞时长 = `min(请求的 timeout_sec, 120s)`。到点未达终态就**正常返回**一个
`{"timed_out": true, "tasks": [...当前状态...], "hint": "call await again to keep waiting"}`。
语义从「等到好为止」变成「等一会儿,并且一定给你一个诚实的快照」。

**代价**:长任务下 coordinator 要多调几次 await(每次都是一轮 LLM 开销)。用 120s 而不是 30s 就是在
平衡这个。停滞检测(D8)也会借这条路径把告警塞回给 coordinator。

---

## D6 `[偏离]` MCP 注入走 ACP `mcpServers` 参数,不写 `.mcp.json`

**犹豫**:设计稿 §5.2 给了两个方案,让「实现时以实测为准」。

**查到**(读 `.acp/node_modules/@zed-industries/claude-code-acp/dist/acp-agent.js`):
- 适配器 `initialize` 里明确 advertise `mcpCapabilities: { http: true, sse: true }`;
- `newSession` 会把 `params.mcpServers` 里带 `type` 的条目原样翻成 Claude Code SDK 的 http/sse MCP 配置,
  含 headers。

**决定**:走 ACP `session/new` 的 `mcpServers`,HTTP transport,token 放 header。
**不**往 agent home 写 `.mcp.json`。

**理由**:agent home 是跨 run 持久的。往里写一次性 token 意味着 token 会泄漏到后续 run,
而且并发跑同一个 agent 的两个任务会互相覆盖配置文件。会话级参数天然是每会话隔离的,没有这个问题。

---

## D7 `[补洞]` mock backend 在 dynamic 模式下当「真的 MCP 客户端」

**犹豫**:dynamic 模式的一切都建立在「agent 会调 MCP 工具」上,而 mock backend 里没有模型。
那 §8 要求的零成本 E2E-1/E2E-4 怎么跑?最省事的做法是给 engine 开一条测试后门,直接调台账 API——
但那样测的就不是真实链路,MCP 层、鉴权、策略拦截全都绕过去了。

**决定**:mock 的 dynamic 会话是一个**货真价实的 MCP 客户端**(用 `modelcontextprotocol/go-sdk` 的 client),
连 hub 暴露的同一个 HTTP 端点,按脚本依次调 `list_agents` → `propose_plan` → `delegate` ×N → `await` → `finish_run`。
worker 侧同理会调 `report_progress`。

**代价**:mock 变复杂了(多了约 150 行)。换来的是 E2E 真的穿过 MCP 传输层、token 鉴权、策略引擎、台账,
只有「模型的脑子」被替换成脚本。这是我认为唯一诚实的零成本验收。

---

## D8 `[补洞]` 停滞告警怎么「注入」coordinator

**犹豫**:§5.4 说停滞时「向 coordinator 注入一条系统提示」。但 coordinator 要么在自己的回合里跑,
要么阻塞在某个工具调用里,没有第三方能插话的位置。

**决定**:两条腿——
1. 打 `stall_warning` 事件(UI/审计一定看得到,这条不依赖 coordinator 配合);
2. 置一个 pending notice 标志:**下一次任何 hub 工具调用的返回值里都会挂上这条告警**;若 coordinator 正卡在
   `await` 里,则立刻让那次 await 提前返回并带上告警。

**代价**:如果 coordinator 既不调工具也不 await(比如在长考),告警只进事件流不进它的上下文。
兜底是 run 墙钟。

---

## D9 `[补洞]` coordinator 到底能不能读文件

**犹豫**:§7 说 coordinator「不自己干活,它没有文件工具,只有 hub 工具」。可是终审时它需要判断
「验收点是否满足」,而产物在交换目录里——不给读文件工具,它只能靠 worker 自报的 summary 做终审,
这等于让被考核者自己填成绩单。

**决定**:coordinator **只给 `Read` + `Grep`**(只读,不给 Write/Edit/Bash),外加全套 hub 工具。
系统提示里写死:只读工具仅用于**验收核对**,不得用来自己完成任务;要产出东西就 delegate。

**代价**:一个想偷懒的 coordinator 理论上可以用 Read 读一堆东西然后自己写结论进 summary。
护栏挡不住这个(它本来就要写 summary),只能靠提示词。但「终审能看见真东西」的收益明显大于这个风险。

---

## D10 `[补洞]` 深度语义与 `MaxParallel` 的作用域

**犹豫**:`Depth` 说 coordinator=0,那 coordinator 直接委派出去的任务是 0 还是 1?`MaxParallel` 是限
「同时活着的 worker 会话数」还是「同时 working 的任务数」(两者在 input-required 时不一样)?

**决定**:
- coordinator 是 0,它 `delegate` 出来的任务是 **1**;`handoff` 出来的是父任务 depth+1。
  `MaxDelegationDepth=3` 即最多 coordinator → A → B → C。
- `MaxParallel` 限的是**占用会话槽位**的任务数,即 `working` + `input-required` 都算占用
  (会话确实还活着、还在吃内存)。`submitted`(排队中)不占。这与 §5.4「submitted 不启动会话」一致。

**代价**:一堆任务卡在 input-required 等 coordinator 回答时会占满并发额度,吞吐下降。
但这是真实的资源占用,假装它不占更危险。

---

## D11 `[偏离]` `engine.Hub` 更名为 `engine.Broker`

设计稿把新组件叫「loom hub」,而 engine 里已经有一个叫 `Hub` 的 SSE 广播器。两个 Hub 会让后来的人读不懂架构图。
把旧的改名 `Broker`(它本来就是个 fan-out broker),新组件独占 `hub` 这个名字。纯机械改动,涉及 4 处引用。

---

## D12 `[补洞]` 新增两个第三方依赖是否可接受

README 现在的卖点之一是「唯一第三方 Go 依赖」。这版之后是三个:`coder/acp-go-sdk`、`a2aproject/a2a-go`、
`modelcontextprotocol/go-sdk`,间接依赖合计 9 个(实测,无 grpc)。

**决定**:接受,并把 README 的说法改成「三个第三方依赖,全部是协议层」。
理由是设计稿 §10 定的原则——「标准协议不手搓」。MCP 和 A2A 都是有官方 Go SDK 的标准协议,
手写 JSON-RPC 去凑协议兼容,省下的依赖会用双倍的协议 bug 还回来。
`a2aclient`(唯一会拉 grpc 的包)被 D1 排除在外。

---

## D13 `[补洞]` dynamic run 的「重试」与「恢复」

static 模式有 `RetryNode`(终态 run 可从任意节点重跑)。dynamic 模式没有 DAG,重试一个任务在语义上是什么?
上下文(coordinator 会话)已经死了,重跑一个任务没人接它的结果。

**决定**:dynamic run **不提供** per-task 重试,UI 上不显示该按钮。终态 dynamic run 只能整体重跑
(发起新 run)。进程重启导致的 `interrupted` 同理——coordinator 会话不可恢复。

**代价**:少一个便利功能。但假装能重试、结果没人消费,比明确说不能更坏。

---

## D14 `[补洞]` 成本口径的诚实性

ACP 走订阅,真实扣费为零。设计稿要求按 API 牌价折算。这个数字**不是账单**,写进 UI 有被误读的风险。

**决定**:照设计稿折算,但所有展示位一律标注「est. API 等价」,且 `costs.jsonl` 每条都带 `model` 与
原始 `usage`,这样牌价变了可以重算历史。解析失败时成本记 0 并打 `cost_unavailable` 事件——
**不**猜、**不**用默认值补,也**不**因此让节点失败。

---

## D16 `[补洞]` `finish_run` 会杀掉在途任务,包括外部 A2A 提交的

**发现于实测**:开一个 dynamic run,外部 A2A client 通过 `message/send` 塞进一个任务,coordinator 几秒后
`finish_run` —— 那个外部任务被 `canceled` 了。handoff 出去的子任务同理(coordinator 只 await 了自己那两个)。

**犹豫**:两条路。
1. 维持现状:coordinator 的裁决即 run 的终点,在途的一律取消。
2. `finish_run` 先排空:等所有在途任务自然结束再收尾。

**决定**:维持 1,但把取消原因写清楚(「run ended before this task finished; no one remained to consume its result」),
而不是含糊的「run ended」。

**理由**:方案 2 会把 run 的终止时间交给任意一个 worker——最坏情况是被单任务超时(默认 30 分钟)拖住,
而 coordinator 早就判完了。dynamic 模式**唯一**的终止保证是预算,不能在收尾这一步把它让出去。
设计稿 §1 也明确把外部互通列为非目标(「A2A 端点先只监听本机,协议形态为将来外部互通留好」),
所以外部任务在生命周期上是「客人」,这个取舍可以接受。

**留下的坑**:如果将来要认真做外部接入,需要引入「外部任务不计入 run 生命周期」或者「run 排空窗口」
之类的概念。现在不做,但把问题写在这里。

**另外**:handoff 子任务被取消,是因为 mock coordinator 只 `await` 了自己直接委派的两个任务。
真实 coordinator 的提示词里写了 handoff 任务会出现在台账中,且 `await` 不传 task_ids 即等全部——
但这**靠提示词**,护栏挡不住一个不看子任务就收尾的 coordinator。属于设计稿 §10 风险 6「收敛质量」那一类。

---

## D17 实现期间发现并修掉的真实缺陷:并行度会被突破

写 `TestParallelismQueues` 时暴露的,不是测试写错了。

`schedule()` 原本按任务状态统计占用槽位。但 `Exec.StartTask` 是异步的——任务被交给引擎后、worker 会话真正
起来并上报 `working` 之前,它的状态仍然是 `submitted`。于是紧接着的第二次 `schedule()`(任何一次 delegate
或任务完成都会触发)会把它重新算成「排队中」,再派一次,占用数也重新从 0 数起。

后果:`MaxParallel=2` 实际能同时拉起 7 个会话(实测数字)。引擎侧 `d.workers[taskID] != nil` 的去重挡住了
重复执行,所以不会跑重,但**并发上限形同虚设**——真实 backend 下就是同时开一堆 Claude 进程。

修法:引入 `dispatched` 集合,任务从**派发**那一刻起就占槽,直到终态才释放,而不是从「上报 working」开始算。

值得单独记一笔,是因为这类 bug 在 mock 下几乎不可见(mock 任务 10ms 就完成了),只有把并行度当成硬约束
去写断言才会掉出来。

---

## D15 `[补洞]` Sonnet 5 的定价

设计稿 §10 风险 4 提到「Sonnet 5 介绍价 2026-08-31 结束后按 $3/$15 牌价」。今天是 2026-08-01,
介绍价还在有效期内。

**决定**:定价表直接写 $3/$15(牌价),不做时间分段。理由:成本台账的用途是**跨 workflow/agent 横向比较投入产出**,
不是对账;为一个月的介绍价引入「按时间点选价」的逻辑,会让历史数据在 8-31 前后不可比。
在 `ModelCatalog` 的定价字段旁留了注释说明这个取舍。

---

## D18 `[偏离]` backend 三选一拆掉:run 只留 dry-run,运行方式归 agent(用户评审后改)

**来源**:用户评审指出「agent 底层的运行方式应该是它自己的参数,workflow/run 只该决定是不是 dryrun」。

**问题确认**:原来发起 run 时的 backend 下拉框(acp/claude/mock)把两个正交概念挤进了一个枚举——
传输方式(acp vs claude CLI)和真假(mock)。v2 后混淆加剧:dynamic 直接拒绝 claude backend,
一个 run 级选项会因 workflow 的 mode 而非法,说明它放错了层。

**改法**:
- Run 级只剩 `dry_run` 开关(POST body;旧的 `backend:"mock"` 仍被接受并映射过来)。
- Agent 定义新增 `runtime` 字段(frontmatter 持久化),空 = 默认 `claude`。`RuntimeCatalog` 目前只有
  claude(ACP 会话托管),为将来 codex 等留位——加新运行时是注册表条目,不是 schema 改动。
- 引擎 backends 注册表按角色分键:`mock`(dry-run 执行器)、`planner`(CLI 单发,规划专用)、
  `claude`(runtime;acp 适配器缺失时降级为 CLI,此时 dynamic 明确拒绝)。
- 每个节点/任务执行时按 agent.Runtime 解析 backend——同一个 run 里将来可以混编不同运行时的 agent。
- `-backend` flag 弃用但兼容:`-backend mock` 等价于新的 `-dry-run`。
- 旧 run.json 兼容:`EffectiveDryRun()` = `DryRun || Backend=="mock"`。

**没做的**:coordinator 的运行时仍固定为默认运行时(它不是池 agent,没有 runtime 字段可声明);
将来如果 coordinator 也要可选运行时,加在 CoordinatorConfig 上。


---

## D19 `[审计整改]` 验收契约:worker 的信封降级为「声明」,判定权收回引擎

**来源**:对照 `docs/brain-agent-orchestration-audit.md` 自查,B1/B2/B3 三项阻断全部不通过——
任务的 completed/failed 由 worker 信封的 `status` 字段直接决定,而这正是审计点名的
「worker 自述报告是链路上最容易乐观失真的环节」。

**改法**:
- `delegate`/`handoff` 增加必填 `acceptance`:机器可执行检查列表
  (`artifact_exists` / `artifact_contains` / `command`),派单时固定并 schema 校验——worker 无法参与定义自己的及格线(B1)。
- worker 回信封 `status:"ok"` 后,引擎在交换目录**实际执行**这些检查(`hub.RunChecks`);
  全过才 completed,任一失败则任务 failed(kind=blocked)并把检查输出记为证据(B2/B3)。
- 检查结果落在 `Task.AcceptanceResults`,UI 抽屉与 TaskView 均可见。
- 同时增加必填 `constraints`(跨域约束,A4):派单包必须显式补齐 worker 无法自行推断的接口/格式/边界,「none」需显式声明。
- 外部 A2A 提交暂不要求契约(F 组边界加固另行处理)。

## D20 `[审计整改]` 失败类型枚举与按类型路由(E1/E2/E3)

- 信封 error 分支必须携带 `failure_kind ∈ {spec-unclear, blocked, missing-dependency, conflict}`;
  缺失/非法一律记 `unspecified`,且 unspecified **不可返工**(保守方向:不给模糊失败开返工口子)。
- 返工路由由 Go 硬执行:`retry_of` 只在根因任务 kind=blocked 时放行;其余 kind 的返工被结构化拒绝,
  错误文案直接告诉 coordinator 该回规划层改什么。
- 防绕过:不带 `retry_of`、但同 agent + 同 title 复投一个已失败任务,按隐式返工处理,走同一路由。
- 单任务返工上限 `max_reworks_per_task`(默认 2,按返工链根因聚类计数),超限强制升级(改计划或诚实收尾)。

## D21 `[审计整改]` coordinator 轮次化:无状态决策 + 审计实读 + 可恢复(B4/D1/D2/D3)

- coordinator 不再是一条长会话:引擎按**轮**驱动,每轮开新会话,上下文由
  「目标 + 自存便签 + 任务台账快照 + 上轮以来的落定变化 + 预算余量」重建(`hub.RoundPrompt`)。
  单轮上下文随任务树大小走,不随轮数增长(有测试断言)。跨轮记忆走 `record_note`(外置、限 20 条)。
- 轮与轮之间由 `AwaitRound` 唤醒:有任务落定(含同状态下的新反问,按 fingerprint 判重)、有系统通知、或台账空转。
  连续两轮台账零变化且无 verdict → 判 coordinator 卡死,run failed;200 轮与整体墙钟双兜底。
- coordinator 会话工具清零(原 Read/Grep 拿掉),唯一读产物通道是 hub 的 `inspect` 工具——有审计、有计数;
  `finish_run(succeeded)` 在「有产出却零 inspect」时被硬拒(B4 的机制化)。
- 因为状态全部外置,dynamic run 可恢复:重启后 `RecoverInterrupted` 把在途任务判 failed(blocked,可返工)、
  已验收任务原样保留,`POST /api/runs/{id}/resume` 从台账续跑;审批门的放行持久化在 `run.plan_approved`,恢复不再重复审批。

## D22 `[审计整改]` 独立校验者与种子池重建(A1/A3)

- Agent 新增 `independent` 标志:dynamic 派单时对其禁用 `context_hint`(机制拒绝,不是提示词劝阻),
  static 模式的节点 prompt 只给它上游**产物路径**、不给上游自述摘要——fresh context 是机制保证的。
- 种子池按「任意两个 agent 的 (模型, 工具白名单) 必须有实质差异」重建:
  researcher(sonnet, 无工具)/ architect(opus, RW)/ implementer(sonnet, RWEB,实现+自测合并,原 tester 并入)/
  reviewer(opus, R, independent)/ doc-writer(haiku, RW)。原 poet / literary-translator / prosody-auditor
  等仅 prompt 差异的名义拆分随数据清空移除。

## D23 `[补洞]` 工具白名单曾对只读工具与 Task 失效;用 deny 规则封死

**来源**:真实 run 中 coordinator(工具白名单为空)被观察到调用 `Task`(Claude Code 自带子agent)与
`Bash find` 自行探索代码仓库。

**根因**:loom 的白名单靠回答 ACP `session/request_permission` 实现,但 Claude Code 对只读工具
(Read/Grep/Glob)与 Task **默认不发权限请求**——询问面拦截对它们是空集。A2 的"机制层隔离"在这条路径上
名不副实(此前的线级测试用的是显式请求权限的假 agent,恰好没覆盖这个缺口)。

**改法**:ACP 会话 spawn 前,按 agent 白名单生成 Claude Code 原生 `permissions.deny` 规则,写入会话 cwd 的
`.claude/settings.local.json`(loom 托管、每次开会话重写、写失败则拒绝开会话)。deny 由 Claude Code 内核强制,
不走询问、提示词不可绕过。`Task` 无条件在 deny 列表——台账之外的子agent 编排永远不是可授予的能力。
loom 的 MCP 工具(mcp__loom__*)不受影响;原有的权限应答白名单保留作第二道。
coordinator 白名单为空 → 全部能力被禁,只剩 hub 工具:**派活成为它唯一能做的事**。

---

## D24 `[补洞]` ACP terminal:Bash 的真实通道

**来源**:真实 run 中所有带 Bash 的 worker 在 `[tool:execute] Terminal` 处无声死亡(无信封判失败)。
claude-code-acp 通过 ACP terminal 方法执行 Bash,而 loom 的 client 只有报错 stub 且不看 capability 声明。

**改法**:client 真实实现五个 terminal 方法(每 terminal 一个 OS 进程、有界输出缓冲按协议从头截断、
诚实退出码/信号、会话关闭统一收割),`Initialize` 声明 terminal capability。第二道防线:白名单无 Bash
的会话(coordinator)在 `CreateTerminal` 即拒。单测覆盖成功/非零退出/kill/截断/拒绝。

## D25 `[补洞]` 审批门异步化 + dynamic run 单锁化

**来源**:真实 run 时间线上任务创建早于"initial plan approved"事件 4.5 分钟。根因链:propose_plan 阻塞等人 →
MCP 客户端超时切断工具调用 → 批准信号落入已死等待通道;并且 `run.Events` 多 goroutine 裸写(数据竞争)。

**改法**:审批门改为纯状态机——propose 立即返回并要求结束回合,Approve/Reject 翻转状态 + 注入 notice 唤醒
下一轮;拒绝后可修订重提。dynamic run 的全部状态(事件/任务/对话/协调器卡片/成本)统一收进 `rs.mu` 单锁,
引擎经 `AppendEvent`/`UpdateCoordinator`/`RecordCoordinatorCost`/`TaskSnapshot`/`AcceptanceOf` 加锁访问;
`-race` 全绿。回归测试钉死"propose 返回后、批准前,delegate 依旧被拒"。

## D26 `[补洞]` 契约可行性校验与修约机制

**来源**:coordinator 给只读 reviewer 派了 artifact_exists 契约(必败),发现后口头叫 worker"忽略验收"——
它无权豁免,引擎照判失败,高价值产出淤积在消息流里。

**改法**:派单与修约时校验可行性——artifact 类检查要求 agent 具备 Write/Edit,否则结构化拒绝;
新工具 `amend_acceptance` 允许修订在途任务的契约(校验同派单、不允许空契约=不可豁免、入审计事件、
worker 下一轮次边界收到通知);引擎判定时按修订后契约加锁读取。提示词明确:"你不能豁免,只能修约"。
配套:reviewer 白名单放宽为 Read,Grep,Glob(可自主发现文件);消息通道仅用于协调、产物必须落文件;
外部事实先派廉价核实任务;coordinator transcript 跨激活追加不覆盖。

## D27 `[约定]` 产物目录:~/workflow-output/<主题名>/(已被 D36 取代)

dynamic run 的交换目录本体就是 `<output根>/<短名>/`(`-output` flag / `LOOM_OUTPUT`,默认 ~/workflow-output)。
短名由 coordinator 按主题起(`name_output` 工具或 `propose_plan.output_name`,kebab ≤40 字符,重名自动 -2 后缀);
**首个任务派发时冻结**,未起名自动兜底 `MMDD-<runid短>`。产物外部实时可见;删除会话不删产物目录;
旧会话(已有任务)保持原交换目录不迁移。static 模式维持内部交换目录不变。

## D28 `[体验整改]` coordinator 持久会话 + 对话历史回灌(修订 D21 的每轮重建)

D21 的「每轮全新会话」把跨轮记忆全押在 `record_note` 上,实用中 coordinator 记不住用户在会话里
说过的话,用户被迫每轮重复关键信息;每轮冷启动 + 全量重建也拖慢了轮次。修订为:

- **一次激活一条活会话**:轮与轮之间复用同一 ACP 会话,后续轮只投递增量
  (`hub.ContinuationPrompt`:落定任务的台账视图 + 用户新消息 + 预算余量 + 一行 goal 锚点)。
  记忆与推理在激活内原生连续,依赖运行时自身的上下文管理/compaction。
- **台账降级为恢复路径,而非唯一状态**:活会话 prompt 失败(适配器崩溃、上下文溢出)时引擎丢弃会话、
  记审计事件、用全量 `hub.RoundPrompt` 重建重试该轮;未送达的用户消息与系统通知重新排队,不丢。
  重建会话自身再失败才判 run failed。进程重启(resume)与会话重开(reopen)天然走同一重建路径——
  D21 的可恢复性保持不变。
- **对话历史回灌**:重建轮的 `RoundPrompt` 新增「Conversation so far」——完整 user↔coordinator
  对话(`run.Chat`),带界限:尾部窗口 40 条、user 单条 2000 字符、coordinator 单条 1000 字符,
  更早的折叠成一行省略计数。**worker 往来永不进对话历史**:任务只以台账 `TaskView` 的
  summary/error/question 出现,transcript 留在审计层。「prompt 不随轮数增长」的断言保持成立
  (随任务树 + 对话尾部走)。
- `record_note` 语义收窄:记台账和对话都复原不了的东西(策略、死胡同、决策),不再是唯一记忆通道。
- 测试:多轮激活恰好开 1 条 coordinator 会话;注入会话中途死亡 → 重建后 run 照常收敛,审计留痕;
  历史节的排重(新消息不重复出现)、截断与窗口折叠;continuation prompt 不带全量台账/便签/历史。

## D29 `[体验整改]` 决策门、约束出处、异议通道、项目记忆(poe2_trade 复盘)

poe2_trade run 暴露的两类失败(用户要求"4 个 demo 让我选"被 coordinator 用"更好方案"越权直接集成;
"keep poe.league as-is" 这条凭空发明的冻结约束把 league→通货耦合的修复锁死,worker 眼看着问题却被合同
禁止修,还自行发明了 30min 轮询)。四项对策:

- **用户保留决策门(prompt)**:用户话里出现"让我选/review 后再改/先看再定",即为硬门——staged 产物
  (不并入目标项目)+ ask_user + 停轮;发现更好方案只能作为额外选项,越权替用户决定按失败论。
  此类等待不算 stalling,不占 one-ask-round 限额。
- **冻结性约束需出处(prompt)**:"keep X as-is" 类约束必须引用用户原话或侦察产物;对既有代码的改动
  先派廉价 impact survey,指令与约束都从 survey 写。凭想象冻结架构 = 把 worker 锁在该修的东西外面,
  而验收检查全绿。
- **worker 异议通道(机制)**:`report_result` 新增 `observations` 字段 → 落 Task → TaskView 透出进
  round prompt。"按规格完成但规格似乎不对/有未提及的耦合/我发明了某个默认值"从此有话筒;
  coordinator 被要求逐条读("completed with observations 往往意味着规格而非工作需要修")。
- **项目级持久记忆(机制)**:交换目录 PROJECT.md,coordinator 用 `record_project_fact` 追加(审计留痕),
  内容注入 coordinator 重建轮与每个 worker prompt(4KB 截断,保头部)。记领域约束、约定、用户纠正;
  run 级策略仍走 record_note。用户可直接编辑该文件。
- 顺带修复 CoordinatorPrompt 审查发现的矛盾:「工具被拒→原轮修正重试」曾把 budget 拒绝也包进去,
  与「预算拒绝→收敛勿绕」直接冲突,现明确 budget 拒绝是唯一例外;record_note 工具描述里残留的
  "每轮无记忆"措辞(D28 前)更新;Deliverable folder 与 Planning 节的重复瘦身。

## D30 `[体验整改]` path-deny + pair 模式(常驻 implementer)

**path-deny**:工具级白名单不画文件系统边界——有 Write 的 worker 能改任何 agent 的 AGENTS.md/agent.md、
workflow 配置、run 台账,乃至自己的 settings 锁(自我修改与跨 agent 注入通道,见 AGENTS.md 讨论)。
顺着既有的 jail 生成器加路径级条目:Write/Edit/MultiEdit/NotebookEdit 对
`<data>/workflows/**`、`<data>/agents/*/agent.md`、`<data>/agents/*/home/.claude/**`、
`<data>/runs/*/run.json`、`<data>/**/{AGENTS,CLAUDE}.md`、`**/.claude/settings.local.json` 一律 deny。
边界按路径不按树:agent home 是合法草稿区,只锁控制面。Bash 无法被路径规则约束——对有 Bash 的
agent 这是与直接用 Claude Code 相同的信任边界,诚实声明而非假装沙箱。coordinator 每次开新会话前
清除 cwd 中被投放的 AGENTS.md/CLAUDE.md(loom 从不往 run workspace 写它们,出现即注入)。

**pair 模式**:对"单仓库迭代开发",逐任务冷启动的 worker 形态是智能差距的主因之一(poe2_trade 复盘)。
`workflow.pair_agent` 指定一个池 agent 为常驻 implementer:
- 它的所有任务在**一个持久会话**串行执行(pairMu 串行化;会话开在 run 级 ctx 上,跨任务存活);
  cwd = 交换目录 = 项目目录,项目的 CLAUDE.md/结构认知原生累积。
- 凭证动态绑定:RolePair token 不携带 taskID,每次工具调用经 `RunSession.PairTask()` 解析当前任务
  (引擎在每个 pair 任务的回合前后 Set/清除)——report_result 永远落在当前任务,审计归属不乱。
- 会话跑 agent 默认模型(单会话无法逐任务切模型,coordinator prompt 已声明);prompt 失败 → 任务照常
  按错误落账,会话丢弃,下一个 pair 任务重建(冷启动是故障的代价,不是设计的代价)。
- 验收契约/预算/审计与普通 worker 完全一致;额外授予 record_project_fact。
- CoordinatorPrompt 增 pair 节:实现类任务路由给它、指令引用用户原话、并行扇出仍走普通 worker。
- 引擎在 StartRun/reactivate 校验 pair agent 必须在池内;UI 设置页(dynamic 表单)提供下拉选择。

## D31 `[体验整改]` 反馈闭环与两级记忆(feedback → prompt;MEMORY.md;修订提案)

D29 建了记忆的 v1(PROJECT.md + observations),但反馈没有落点、agent 没有自己的记忆、
"复盘 → 改 prompt" 仍是人肉环节(poe2_trade 的教训是手写进 D29 的)。四项对策:

- **PROJECT.md 截断改保头+保尾**(修 D29 隐患):原实现超 4KB 保头弃尾,而用户纠正恰好追加在尾部——
  文件一超限,被保留的是旧事实、被丢掉的是最新纠正,与 "纠正要立刻记录" 的意图正对着干。
  改为头(奠基约定)+ 尾(最新纠正)各留一段、中间省略,按行边界切(`clipHeadTail`,有测试断言)。
- **run 反馈落点(机制)**:`Run.feedback` + `POST /api/runs/{id}/feedback`(仅限非活跃 run——活跃时
  会话聊天就是通道)。同 workflow 历史 run 的反馈(最近 3 条、每条 1KB 截断、带日期与 goal 首行)注入
  coordinator 系统 prompt 与 static planner prompt 的「User feedback on previous runs」节;prompt 指示:
  反馈高于默认、含持久事实则转存 record_project_fact、指向 agent 定义缺陷则走修订提案。
  **worker 永远看不到反馈原文**——main agent 负责把它翻译成指令;持久事实走 PROJECT.md。
  UI:会话终态尾部与 run 详情页均有反馈框(static run 也覆盖)。
- **agent 手艺记忆 MEMORY.md(约定+机制)**:agent home 本就跨 run 持久且在 path-deny 边界之外
  (home 是 agent 自留地),补上机制:home/MEMORY.md 由 agent 自己维护(AGENTS.md 契约新增条目,
  仅对有 Write/Edit/Bash 的 agent 生成——write_artifact 只达交换目录,写不了 home),loom 把其内容
  注入该 agent 的每个任务 prompt(dynamic WorkerPrompt + static node prompt,2.4KB 截断偏尾部)。
  与 PROJECT.md 的分工:PROJECT.md 记"这个项目"的事实,MEMORY.md 记"这门手艺"的经验。
- **修订提案 propose_agent_amendment(机制)**:coordinator 发现 agent 的**常设定义**(而非本次 spec)
  导致会复发的失败时,提交 Amendment(rationale + 完整替换 prompt + 当前 prompt 快照),存
  `data/amendments/`,状态 pending——**只创建待审记录,什么都不改**。人在 agents 页审阅
  (当前/提议对照 + 理由 + 来源 run)后批准才经 SaveAgent 应用(AGENTS.md 同步重生成);
  快照与现值不符即拒绝应用(stale:提案推理所依据的文本已不存在,不许覆盖人的更新编辑)。
  path-deny 不变量原样成立:agent 出证据,人改身份;自动自我改进被明确排除——
  反馈可以自动收集、自动提案,生效必须过人。

## D32 `[体验整改]` 反馈改对话式:复盘由 coordinator 消化,原文不再直接注入(修订 D31)

D31 的反馈是"原文落库、原文注入",指代会失效("那个表格"到下个 run 没有指称对象)、没有澄清
通道、用户得不到"被理解成什么"的确认。修订为**复盘对话**:

- 对真实 dynamic run 提交反馈 = 以 postmortem 身份重开会话(`ReopenFeedback` → 复用 reactivate 管道,
  `ChatMessage.Kind="feedback"` 全程携带)。round prompt 中反馈消息不进「New messages」,而有专属
  「Post-run feedback (POSTMORTEM — digest, do not resume work)」节,指示四步:先**理解**(指代从
  对话/台账/产物里解析,真歧义就反问并停轮)→ **沉淀**(事实走 record_project_fact、定义缺陷走
  propose_agent_amendment)→ **蒸馏**(新工具 `conclude_feedback` 把教训改写成自包含的常设纠正,
  落 `run.feedback`)→ 回复用户记了什么。明确**不许因反馈自行派活**——返工需求由用户说了算。
- 未来 run 注入的是**蒸馏版**;用户原话留在 run.Chat 审计(聊天里带「复盘反馈」标记)。
- 边界:static 模式没有对话角色、dry-run 的 coordinator 是脚本——两者保留原文字段(UI 文案如实
  声明);空文本仍是"清除已存结论",不唤醒任何人;活跃 run 照旧拒绝(会话聊天就是通道)。
- 代价:每条反馈一次 coordinator 激活。换来的是反馈在写入前被解析、确认、结构化——
  "我说什么就存什么"不再是这个环路的形态。

## D33 `[体验整改]` 复盘产物分级:记录归档、规范过人、注入只走确认(修订 D32)

D32 的蒸馏结论仍是**一段自由文本直接注入**——实践里 coordinator 常把它写成事件经过的复述,
下个 run 开局收到的是一段与新目标无关的叙事 noise;且"什么进入之后每次 run 的 prompt"这个
决定完全没有过人。修订为**两级产物 + 确认门**(与 amendment 同构:agent 出证据,人定生效):

- **复盘记录(存档层)**:`run.feedback` 语义降级为本次 run 的复盘结论存档——coordinator 经
  `conclude_feedback(text)` 写入,UI 可读可编辑,**永不注入**。原始对话照旧留在 run.Chat。
- **行为规范 Lesson(注入层)**:`conclude_feedback` 新增 `rules[]`——从复盘中提炼的具体做法,
  每条必须是自包含的祈使句("报告先给结论"),单条 ≤400 字符、单次 ≤5 条(超限拒绝:规范是
  指令不是复述)。落 `data/lessons/`(workflow 级,带来源 run),状态 pending——**只创建待审
  记录,什么都不注入**。反馈不含可沉淀做法就一条都不提,合法且被 prompt 明确鼓励。
- **确认门**:workflow「复盘」面板分层展示——待确认规范(采纳/改后采纳/不采纳)、生效中规范
  (可编辑/移除,也可手动添加:人写的即人批的,直接生效)、复盘记录(仅存档)。API 走
  `/api/lessons` 族,与 amendments 对称。
- **注入**:`LessonsFor` 只取该 workflow **approved** 的规范(最新优先,上限 20 条),注入
  coordinator 系统 prompt 与 static planner prompt 的「Standing rules of this workflow
  (user-confirmed)」节。pending/rejected 与复盘记录永远到不了 prompt;worker 照旧看不到。
- 边界:static/dry-run 无对话角色,反馈原文只作复盘记录(不再像 D31/D32 那样直注),要注入
  的规范由用户在面板手动添加;既有 run.feedback 存量自动降级为存档,不迁移——它们本来就是
  这次要清除的 noise,值得留的由用户提炼成规范再生效。
- 不变量:从复盘到注入的唯一路径是用户确认;自动收集、自动提案可以,自动生效不行。

## D34 `[体验整改]` 规范增长治理:新规范顶替旧规范,超阈值发起整理(补全 D33)

D33 的规范集只进不出,增长模式可预见:同一条教训被反复复盘出略有不同的版本("结论先行"/"别把
结论埋最后"/"先给 TLDR"),按"最新优先截 20 条"处理增长是错的语义——每条都是用户亲手确认的,
静默挤出第 21 条等于系统单方面撤销人的决定。治理分两手,均不引入自动删除:

- **顶替(supersede)**:`propose_rules` 从 conclude_feedback 拆出为独立工具(结论归记录、规范归
  提案,消歧;整理场景也复用它)。每条提案可带 `replaces[]`——注入 prompt 的规范节现在带 id,
  postmortem 提示词明确:与现有规范重叠的新规范**必须顶替而非追加**。快照/陈旧拒绝与 amendment
  同构:SaveLesson 时快照被替目标的文本,批准时任一目标已被用户改过或已不存在 → 拒绝并要求重新
  提案;批准即原子换入换出(被替文本留在提案记录里作审计)。`replaces` 非空 + 空文本 = 纯退役
  (删目标、不新增)。UI 待确认区按三种形态渲染:新增/替换(旧文划线 → 新文)/退役。
- **整理(consolidate)**:生效规范 ≥12 条(`lessons_nudge`,上限 20 之前就提醒——臃肿的规范集
  在溢出之前就已经在拖累每个 prompt)时「复盘」面板出提示,「发起整理」= 以 `ChatConsolidate`
  身份唤醒该 workflow 最近一个已结束的真实 dynamic 会话(run 只是载体,规范是 workflow 级的;
  dry-run 的脚本 coordinator 读不懂规范集,跳过)。round prompt 给专属 MAINTENANCE 框架:只许
  propose_rules(合并=一条新规范 replaces 多条、改写=新文 replaces 一条、退役=空文),优先解决
  互相矛盾的条目,健康的规范不许碰,禁止派活、禁止 conclude_feedback(没有 run verdict)。
  产出照旧全体 pending,逐条过人。
- 不做的:按命中率衰减(无法可靠归因一条规范是否起效)、按时间过期(行为偏好不随时间失效)。
- 不变量重申:提案可以自动,合并、退役、生效必须过人;陈旧提案不许覆盖人的更新编辑。

## D35 `[提示词]` dynamic 实现里程碑的独立评审:建议而非门禁(已被 D37 收紧为门禁)

dynamic 模式此前对质量只有两道闸:验收命令(引擎跑,挡"跑不过")与 coordinator 的 inspect
纪律(零 inspect 拒绝 finish_run)。但 coordinator 的 inspect 不是独立评审——它已读过作者
汇报,视角被叙述污染;实现类里程碑要不要过 reviewer 全凭它当轮想起与否(pair 模式下 implementer
自写自测,全链路可能没有第二双眼睛)。

对策只在提示词层:coordinator prompt 新增「Independent review (your judgment, not an engine
gate)」节——仅当池内确有 `independent` agent 时渲染(建议一个无人能执行的评审是噪音)。内容:
实质性实现里程碑在接受前应委派独立评审、高严重度发现按 blocked 走返工、验收命令证明"能跑"
而非"做对";机械/低风险工作可跳过,但跳过即是在决定"机器检查 + 被污染的自读"已经足够。
**明确不做机制强制**(如 require_review 开关):评不评审是 main agent 的判断,引擎的门禁
保持验收 + inspect 两道不加码——机制约束留给确有复发证据之后再议。

## D36 `[约定]` 一个 run 一个工作区:用户选的目录既是项目也是产物落点(取代 D27)

D27 的两套目录(用户选的 project workspace + coordinator 起名的 `~/workflow-output/<短名>` 交换目录)
在实战里出了两次事:空目录起新项目时 coordinator 把整个 app 当"产物"建到了交换目录;用户抱怨后,
它又把"workspace"理解成自己会话的 cwd(`~/.loom/data/runs/<run>/workspace`,恰好叫 workspace),
让 worker 把项目复制进了 loom 的内部目录——用户选的目录始终是空的。加上 pair 会话 cwd 是项目、
但 prompt 却说"当前目录是你的私有工作区",implementer 把 craft MEMORY.md 写进了用户项目。

改为:**一个 run 只有一个目录,叫工作区**——`Run.Workspace`,由用户在会话里选(不选=默认根
`~/workflow-output`,`-output`/`LOOM_OUTPUT`),项目与产物都在里面;coordinator/worker prompt、
工具描述、验收路径统一只说 workspace,"exchange directory / output folder / name_output /
propose_plan.output_name" 全部移除,coordinator 也不再被要求确认交付位置。coordinator 会话 cwd
改为 `runs/<run>/coordinator`(私有、不叫 workspace);worker prompt 把"私有 home"和"工作区"分开
写明,pair 会话 cwd=工作区。UI 选择器:默认工作区显式展示、可"使用默认工作区"、文件夹浏览里可
**新建文件夹**(创建即选中)并对**空文件夹**就地重命名(服务端同样只允许重命名空目录、限 $HOME 内,
MRU 历史跟着改名)。旧 run 兼容:有 `output_dir` 的以它为工作区继续(工作在那里),更旧的回退内部目录。

## D37 `[机制]` 完成判据四道门:测试契约、真实路径、目标证据、独立评审

newspush run 的复盘:13 个任务的验收全是 artifact_exists/contains + go build/vet/gofmt,项目 0 个测试;
E2E 由 coordinator 自己写成 `send --dry-run`,拿 "7/7 PASS" 当结论;邮件一封没发出去仍 finish_run(succeeded);
池里的 reviewer 一次没用。根因:loom 验证的是"契约过了",不是"目标达成了",而写契约的正是被验证的人。
四道门全部在引擎层,不靠提示词自觉:

1. **契约必须含测试**(`hub.ValidateChecks`):acceptance 中出现 build/lint 类命令(go build/vet、gofmt、npm run
   build、tsc、cargo build、make build…)而没有 test 类命令(go test、npm test、pytest、cargo test、make test…)
   → 派单/修约被拒,错误信息点名哪条检查、要补什么。纯文档任务(无 build 检查)不受影响。
2. **禁止 dry-run 式验收**:acceptance 命令含 `--dry-run/--dry_run/--mock/--no-send/--simulate/…` → 拒绝。
   验收跑真实路径;真实路径需要用户才有的东西(凭据/账号)就先 ask_user;拿不到就是"未达成",不是"验证通过"。
3. **目标证据(definition of done)**:新工具 `declare_evidence`(或 `propose_plan.evidence`)在**第一次 delegate 前**
   必须声明"目标达成的可观察证据"(≤12 条,如"收件箱收到一封 digest 邮件");证据项可标 `needs_from_user`,
   标了就必须先 ask_user 才能 delegate。`finish_run(succeeded)` 必须对每条证据报 `met + how`,缺报/未 met/
   met 无 how 均拒绝——只能 `failed` 并说明缺口(failed 永远允许)。证据与结果持久化在 `Run.Evidence`,
   round prompt 每轮回显,UI 会话面板显示 "完成判据 n/m 已验证"。
4. **独立评审门禁**(收紧 D35):池内有 `independent` agent 时,`finish_run(succeeded)` 要求最后一个"实现类"
   已完成任务(agent 工具含 Edit/Bash)之后存在一个 independent agent 的已完成任务;评审早于最后一次代码改动
   即视为过期。仅 Write 的文档类任务不算实现、也不使评审过期。池内无 independent agent 则不适用。

mock coordinator 同步遵守协议(declare → delegate → inspect → finish 带 evidence)。

## D38 `[机制]` Hook 网关:工具面按身份、scope、level 实时判定,reason 原文回给模型

飞行员改造(见 loom-pilot-plan)第一阶段。动机:模式(solo/pair/orchestrate)若只靠提示词,主 agent 必滑向
"全部自己做";要让它"不能不按",判定必须落在工具调用上。已用真实运行时验证(`internal/llm/live_hook_test.go`,
`live_gate_test.go`,`LOOM_LIVE_REPRO=1`):ACP 适配层执行会话 cwd settings 里的 PreToolUse/PostToolUse command
hook;`bypassPermissions` 下 `permissionDecision: deny` 仍生效;`permissionDecisionReason` 原文进模型上下文;hook
拿到 `tool_input`(文件路径、Bash 命令行);hook 子进程继承适配层进程环境变量。

机制:
- 每个 loom 会话的 jail(`.claude/settings.local.json`)多写两条静态 hook:`'<loom 二进制>' gate`(PreToolUse 匹配
  全部受管工具,PostToolUse 匹配写类工具 + Bash)。**凭据不落盘**:`LOOM_GATE_URL/LOOM_GATE_TOKEN` 走会话进程环境,
  hook 继承——同一 cwd 的并发会话(pilot + pair、同一 agent home 的并发任务)互不串身份。
- `loom gate` 子命令把 stdin 的 hook JSON 转发到 `POST /gate`(Bearer token),原样打印应答;**fail-open**:
  无凭据/无服务/超时/非 200 → 不打印、exit 0,调用放行(引擎挂了 run 也就死了;loom 之外的 Claude Code 打开同一
  workspace,hook 因无凭据而惰性)。
- `hub.Gate`(`internal/hub/gate.go`)按 token → RunSession + identity 判定,顺序:① 身份绑定的工具白名单(受管
  工具表 `model.ToolGrants` 与静态 jail 共用一份;Task 永远拒绝并指向 delegate);② loom 自身控制面(workflow 文件、
  agent.md、agent home 的 .claude、run.json、数据目录下的 AGENTS.md/CLAUDE.md、任何 settings.local.json)任何会话不写;
  ③ 任务 scope 所有权(`Task.Scope`,delegate 时声明,相对 workspace 的文件/目录前缀;worker 写 scope 外被拒、任何人写
  他人在飞 scope 被拒,任务 settled 即释放);④ run 的 level:ORCHESTRATE 下 main agent 写 workspace 被拒(reason 指向
  delegate),Bash 明显写操作(重定向到文件、tee、sed -i、rm/mv/cp/mkdir/touch、git 变更类子命令、包管理 install/add…)
  同样被拒,验证类命令放行;worker 的 shell 仍是信任边界。PostToolUse 记 `Run.Writes`(谁用什么工具写了哪条
  workspace 相对路径 / 谁跑了写类 shell),供评审门归因(上限 400 条)。
- `Run.Level/LevelSource/LevelLog`,`RunSession.SetLevel(level, source, reason)`;legacy run 无 level 视为 orchestrate
  (旧 coordinator 本来就没有手)。
- jail 写入改为**区分归属**:agent home(数据目录下)是 loom 的文件,整体重写;用户 workspace 的 settings.local.json
  **合并不覆盖**——只加 loom 的路径规则与 hook 条目(精确记账),工具级 deny 不写(由网关按身份判,避免共享 cwd 的两个
  会话互相继承 jail),最后一个 loom 会话关闭时精确移除 loom 条目(loom 创建的空文件/目录一并删除)。无网关凭据的会话
  仍写完整 deny(不比从前弱)。

## D39 `[机制]` 飞行员:main agent 住进工作区、有手;level 决定何时能动手;常驻伙伴可多选

飞行员改造第二阶段(取代 D-早期 "coordinator 无文件工具" 的设计)。动机:用户真正想要的是 Claude Code 的体验
(直接和干活的那个会话说话、流式可见、随时打断)加上 loom 的显性治理;而"说话的人没有手、只能派单"是体验差的根源。
拆法:**说话的人**(main agent)可以有手,**守规则的人**(引擎的门 + hook 网关)决定手什么时候能用。

- main agent 会话 `WorkDir = run workspace`,工具 = `CoordinatorConfig.Tools`(空 = 全部 `model.DefaultPilotTools`),
  MCP = hub;转录仍落在 `runs/<id>/coordinator/`。项目自己的 CLAUDE.md/AGENTS.md 对它生效(和任何住在那里的会话一样)。
- `Run.Level`(solo | pair | orchestrate)开 run 时由 `CoordinatorConfig.Level`(钉死)或默认 `solo` 写入,来源记入
  `LevelSource/LevelLog`;用户可随时改(`POST /api/runs/{id}/level`,活 run 下一次工具调用生效并以 notice 告知 main
  agent;闲置 run 存下供下次激活)。main agent 不能自己改 level(后续 triage 只能申请升档)。
- 网关(D38)按 level 放行/拒绝 main agent 的写;orchestrate = 手被绑(Edit/Write 进 workspace、写类 shell 均拒,reason
  指向 delegate)。评审门(D37 第四道)把 main agent **自己**写的代码(`Run.Writes` 中 by=coordinator 且非文档)也算实现
  改动:最后一次改动之后必须有 independent agent 的已完成任务,否则 finish(succeeded) 拒。文档(.md/.txt/docs/)不算。
- 常驻伙伴 `Workflow.PairAgents []string`(legacy `PairAgent` 并入,`EffectivePairAgents()`):每个伙伴一条持久会话住在
  workspace,各自的任务在自己会话上串行;implementer 与 reviewer 可同时常驻;hub 凭据 `IssuePairToken(run, agent)` 带
  agent 名,`SetPairTask(agent, task)` 按伙伴绑定。
- 流式:`SessionRequest.OnText` 逐块回传正文 → `CoordinatorState.Draft`(≤4 次/秒发布);工具行 → `CoordinatorState.Trace`
  (本轮,上限 40);round 结束 `CoordinatorReply` 提交正文、清空 draft/trace。UI 会话区显示"正在输入…"与本轮动作。
- 提示词:coordinator prompt 改为飞行员身份——"你的手与 level"段说明三档含义与网关行为;验证纪律:可以直接读,但
  finish 门只认 inspect 的审计读;workspace 段:cwd 即 workspace;常驻伙伴段列出每个伙伴及角色。round prompt 顶部回显
  level 与一句含义。

## D40 `[机制]` 监听 + 评估 + Triage:level 由引擎从结构化评估算出,main agent 只能申请升档

飞行员改造第三阶段。前提(D38/D39):level 能被网关强制;本阶段解决"level 由谁、何时、凭什么定"。结论:不交给
main agent 自选(必滑向 solo),而是**强制它交评估、引擎按阈值判**。

- **评估强制**:`RunSession.assessPending` 为真时,main agent 对 workspace 的写(Edit/Write/写类 shell)被网关拒、
  `delegate` 被 ledger 拒,reason 都指向 `assess_task`。置为 pending 的三个时机:run 开始(goal 本身就是任务)、
  监听器把新用户消息判为 task、中途重判信号。round prompt 顶部回显 "Assessment: PENDING — 原因"。
- **`assess_task`** 工具:`{summary, steps, modules, parallel_branches, roles, changes_code, est_files}` →
  `hub.Triage`(纯函数)按 `TriageConfig` 阈值出 level 与理由:steps ≥6 / 独立分支 ≥2 / 角色种类 ≥2 / 预计文件 ≥8
  任一 → orchestrate;否则 changes_code 且(配置了常驻伙伴或池里有 independent agent)→ pair(可关);否则 solo。
  结果写 `Run.Assessments`(上限 20)、level 变化记 `LevelLog(source=triage)`、聊天里出一张 system/triage 判定卡。
  **不覆盖**:工作流钉死的 level 与用户在本 run 设定的 level(`LevelSource=user`)一律不被 triage 改动,卡片上注明
  "triage 建议 X,未应用"。
- **`request_level`**:main agent 只能申请比当前更高的 level(source=pilot);降档是用户的事;用户已设定时拒绝。
- **中途重判**:自上次评估起 main agent 自己改过的**不同**代码文件数 ≥ `reassess_files`(默认 8),或 acceptance 命令
  失败次数 ≥ `reassess_test_failures`(默认 3)→ 置 pending + notice(每次评估只触发一次)。
- **监听器**:每条进入活 run 的用户消息,引擎**并发**用 haiku 单轮、无工具分类为 task / continuation / question /
  meta(消息本身立即送达 main agent,不等分类);task → 置 pending + notice。失败 = move on(不阻塞);**连续 3 次**
  起每次失败都在聊天里出 system/notice 提示,成功一次重置。mock 后端按动词判(dry run 可测)。首条消息不走监听
  (按构造即任务);重开 run 的消息走监听。
- 已知取舍:分类与送达并发,新任务判定落地前的几秒内 main agent 可能已经开始写——可接受(写入会被归因,评审门兜底)。

## D41 `[机制]` 模板即任务:静态工作流作为动态 run 的一个 task 运行

飞行员改造第四阶段。静态工作流不再是另一种"会话",而是 main agent 可以调用的**模板**:`list_templates` 列出所有
static 工作流,`run_template{template_id, goal}` 在本 run 的 ledger 里创建一个 `Agent = "template:<id>"` 的 task,
过与 delegate 相同的门(评估、完成判据、审批、任务数预算);引擎 `execTemplateTask` 用现有静态管线(planner 出 DAG →
deterministic execute)在**同一 workspace** 启一个子 run(`Run.ParentRunID/ParentTaskID`,task 侧 `Task.SubRunID` 双向链接),
轮询至终态后以子 run 的结果落定 task(失败记 blocked,可返工;父 run 取消则子 run 一并取消)。动态预算把整个模板算一个
task——模板自身的结构由 DAG 无环保证。UI 任务卡片对模板任务显示"⧉ 子运行"链接到静态 run 页(拓扑页复用)。
mock coordinator 以 `simulate-template` 标记演示该路径。未做(有意推迟):triage 用 LLM 自动匹配模板并让用户确认——
现阶段由 main agent 按 list_templates 的描述自行选择;模板页"用此模板开始"仍是直接启动静态 run。

## D42 `[UI]` 一个会话区;static / dynamic 退为设置(主 agent 一份,静态模板多份)

飞行员改造第五阶段(初版,用了再调)。导航:**会话 / 记录 / Agent 池 / 设置**。
- **会话**:左栏是所有会话(= dynamic run,最新在上,带状态、level、工作区名),右侧是与 main agent 的对话;新会话 =
  工作区选择器 + 第一条消息,一律挂在**唯一**的主 agent 配置(`GET /api/main`:`wf-dynamic`,否则第一个 dynamic
  工作流,都没有就现场建一个)。会话头显示 level 控件(可改)、完成判据、任务树;聊天里有 triage 判定卡、系统提示卡、
  main agent 的流式正文与本轮动作、模板任务的"⧉ 子运行"链接。
- **设置 › 主 agent**:原 dynamic 表单(模型、附加指导、审批、main agent 的手、起始 level、常驻伙伴多选、triage 阈值、
  预算、池);**设置 › 静态模板**:静态工作流列表(运行 / 编辑 / 新建),表单不再有 static/dynamic 单选——模式是记录的
  事实,不是表单的选择;**设置 › 通用**:默认工作区、运行时、演示模式(只读,来自启动参数)。
- **记录**:沿用运行列表;工作流列标"会话 / 模板",带 level 徽章与"⧉ 子运行"链接;run 详情页对子运行显示父运行链接。
- 旧入口 `#/workflows` 重定向到 `#/sessions`;`#/workflows/:id/edit`、`#/workflows/new` 仍可用(模板编辑)。
