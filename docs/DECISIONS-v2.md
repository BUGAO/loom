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

## D27 `[约定]` 产物目录:~/workflow-output/<主题名>/

dynamic run 的交换目录本体就是 `<output根>/<短名>/`(`-output` flag / `LOOM_OUTPUT`,默认 ~/workflow-output)。
短名由 coordinator 按主题起(`name_output` 工具或 `propose_plan.output_name`,kebab ≤40 字符,重名自动 -2 后缀);
**首个任务派发时冻结**,未起名自动兜底 `MMDD-<runid短>`。产物外部实时可见;删除会话不删产物目录;
旧会话(已有任务)保持原交换目录不迁移。static 模式维持内部交换目录不变。
