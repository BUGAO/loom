# loom v2 设计:双模式工作流(static + dynamic/A2A)

> 状态:设计稿 v1(2026-08-01)。未实现。
> 前置讨论结论:图从"预先声明"变为"运行中涌现",但结构单元(A2A Task)依然存在;
> 四条护栏(终止/审批/观测/收敛)从结构保证降级为策略保证,必须由引擎硬执行。

---

## 1. 目标与非目标

**目标**

- Workflow 增加 `mode` 字段:`static`(现状,默认)与 `dynamic` 两种模式并存,共用同一个 agent 池、交换目录、UI 与 ACP 执行层
- dynamic 模式:一个 **coordinator agent** 把控整体进度,通过 **A2A 协议** 把任务委派给池中 agent;worker 完成后向 coordinator 回报;策略允许时 worker 可与交接对象直接交互(peer handoff)
- 所有 agent 间流量经 **loom hub** 路由,任务台账全程可审计
- 预算与策略护栏由 Go 引擎硬执行,不依赖 coordinator 自觉

**非目标(本版)**

- 不做跨机器的分布式 agent(A2A 端点先只监听本机,协议形态为将来外部互通留好)
- 不做人在环中的逐条消息审批(只做批次级审批点)
- 不替换 static 模式的任何行为

---

## 2. 模式对比总览

| | `static`(现状) | `dynamic`(新增) |
|---|---|---|
| 结构 | planner 一次性输出 DAG,预先审批 | coordinator 运行时逐步分解,任务树涌现 |
| 调度 | Go 引擎确定性执行 | coordinator 决策,引擎执行预算/路由/台账 |
| 结构单元 | PlanNode | A2A Task(带生命周期状态机) |
| agent 间交互 | 产物(交换目录+摘要),无消息 | A2A 消息 + 产物;支持 input-required 反问 |
| 终止保证 | DAG 无环(结构性) | 预算硬限(策略性) |
| 审批对象 | 完整计划(含新 agent) | 预算/策略 + 首批任务清单(可选) |
| 适用 | 形状可预判、要复现审计的任务 | 需要收敛循环、反问、边做边分解的任务 |

---

## 3. 架构

```
┌─────────────────────────── loom 进程 ────────────────────────────┐
│                                                                  │
│  ┌─────────┐   MCP(hub 工具集)   ┌──────────────────────────┐  │
│  │coordinator│◄───────────────────►│        loom hub           │  │
│  │ ACP 会话  │                     │  ┌─────────────────────┐ │  │
│  └─────────┘                     │  │ A2A Gateway          │ │  │
│                                    │  │  /a2a/{agent}/...    │ │  │
│  ┌─────────┐   MCP(worker 子集) │  │  Agent Cards         │ │  │
│  │ worker N │◄───────────────────►│  ├─────────────────────┤ │  │
│  │ ACP 会话  │                     │  │ Task Ledger(台账)  │ │  │
│  └─────────┘                     │  ├─────────────────────┤ │  │
│       ▲                            │  │ Policy Engine(预算)│ │  │
│       │ session/prompt             │  └─────────────────────┘ │  │
│  ┌────┴────┐                      └──────────┬───────────────┘  │
│  │ ACP 层   │  coder/acp-go-sdk               │ SSE               │
│  │(现有)  │                                 ▼                   │
│  └─────────┘                            Web UI(任务树)          │
└──────────────────────────────────────────────────────────────────┘
```

**关键决策**

1. **A2A 为 agent 间唯一协议**,用官方 SDK `a2aproject/a2a-go`(实现前先验证 API 现状,不手搓协议层——见 §10 风险)。三协议分工:MCP = agent↔工具,ACP = loom↔agent 运行时,A2A = agent↔agent。
2. **hub 集中托管所有 A2A 端点**:不为每个 agent 起独立进程/端口,hub 在 loom 主进程内多路复用 `/a2a/agents/{name}`,Agent Card 从池定义自动生成(name/description → card;tools/skills → card.skills;capabilities.streaming = true)。逻辑上点对点、物理上全部过 hub → 台账无盲区。
3. **coordinator 与 worker 的"交互能力"都是 MCP 工具**:对 claude 会话而言,委派/回报/反问就是调工具,天然融入 agentic loop;工具实现内部走 A2A client → hub。
4. **worker 处理一个任务 = 一个持续的 ACP 会话**:任务期间会话保活,A2A 的后续消息(追加指令、input-required 的答复)翻译成同一会话的 `session/prompt` 追加轮次。这需要把现有 llm 层从一次性 `Complete()` 重构出 **Session 接口**(§6.1)。

---

## 4. 数据模型

### 4.1 Workflow 扩展

```go
type Workflow struct {
    // ... 现有字段不变(static 模式继续使用 Planner/RequireApproval/Replan*)
    Mode        string             `json:"mode"` // "static"(默认)| "dynamic"
    Coordinator *CoordinatorConfig `json:"coordinator,omitempty"` // dynamic 必填
    Budget      *BudgetConfig      `json:"budget,omitempty"`      // dynamic 必填
}

type CoordinatorConfig struct {
    Model        string `json:"model"`         // 模型目录内
    SystemPrompt string `json:"system_prompt"` // 统筹风格/领域偏好
}

type BudgetConfig struct {
    MaxTasks           int  `json:"max_tasks"`            // 默认 30
    MaxDelegationDepth int  `json:"max_delegation_depth"` // 默认 3(coordinator=0)
    MaxParallel        int  `json:"max_parallel"`         // 默认 3,复用信号量
    TaskTimeoutSec     int  `json:"task_timeout_sec"`     // 默认 1800
    RunTimeoutSec      int  `json:"run_timeout_sec"`      // 默认 7200,硬墙钟
    MaxTurnsPerTask    int  `json:"max_turns_per_task"`   // 单任务会话最大消息轮次,默认 6
    AllowAgentCreation bool `json:"allow_agent_creation"` // 复用现有护栏
    AllowPeerHandoff   bool `json:"allow_peer_handoff"`   // worker 间直接交接
    ApprovalPolicy     string `json:"approval_policy"`    // "none" | "initial"(首批委派需审批,默认)
}
```

### 4.2 Task(台账实体,dynamic 模式的"节点")

生命周期直接采用 A2A Task 状态机,loom 不另造一套:

```go
type Task struct {
    ID          string   `json:"id"`            // task_<ts><rand>
    Agent       string   `json:"agent"`         // 池 agent 名
    Title       string   `json:"title"`
    Instruction string   `json:"instruction"`
    CreatedBy   string   `json:"created_by"`    // "coordinator" | 上游 task id(handoff 血缘)
    Depth       int      `json:"depth"`         // 委派深度,预算用
    Status      string   `json:"status"`        // A2A: submitted|working|input-required|completed|failed|canceled
    Messages    []TaskMessage `json:"messages"` // 全部往来消息(审计)
    Summary     string   `json:"summary"`       // 完成信封摘要(沿用现有信封契约)
    Artifacts   []string `json:"artifacts"`
    Activity    string   `json:"activity,omitempty"` // 实时工具活动,复用现有机制
    Attempts    int      `json:"attempts"`
    DurationMs  int64    `json:"duration_ms"`
    CreatedAt   time.Time `json:"created_at"`
    EndedAt     time.Time `json:"ended_at,omitempty"`
}

type TaskMessage struct {
    Ts   time.Time `json:"ts"`
    From string    `json:"from"` // "coordinator" | task id | "user"
    Role string    `json:"role"` // instruction | followup | question | answer | result
    Text string    `json:"text"`
}

// Task 与现有 NodeState 一样携带成本(API 等价口径,见 §5.5):
//   CostUSD float64   Usage TokenUsage(input/output/cache_write/cache_read)
```

### 4.3 Run 扩展

```go
type Run struct {
    // ... 现有字段;static 模式行为不变
    Mode  string           `json:"mode"`
    Tasks map[string]*Task `json:"tasks,omitempty"` // dynamic 模式取代 Plan/Nodes
    CoordinatorLog string  `json:"-"`               // coordinator 完整 transcript 落盘 nodes/coordinator.md
}
```

dynamic 模式 Run 状态机:`running`(coordinator 会话存活)→ `awaiting_approval`(审批点)→ `succeeded | failed | canceled`。不再有 `planning/replanning`——分解与重排都是 coordinator 的运行时行为,以任务台账留痕。

---

## 5. Hub:工具集、A2A 映射与策略

### 5.1 coordinator 的 MCP 工具集(`loom-hub` server)

| 工具 | 入参 | 行为 |
|---|---|---|
| `list_agents` | — | 池注册表(名称/描述/工具/模型),等价于 Agent Card 目录 |
| `delegate` | agent, title, instruction, context_hint? | 预算检查 → 建 Task(submitted)→ A2A message/send → 返回 task_id,**异步不阻塞** |
| `await` | task_ids[], mode: any\|all, timeout_sec | 阻塞至任务达终态或 input-required;返回各任务状态+摘要/问题 |
| `send_message` | task_id, text | 对 working/input-required 任务追加消息(steering、答复反问) |
| `progress` | — | 台账快照:各任务状态、耗时、深度、预算余量 |
| `create_agent` | 同 static 的 agent 定义 | 复用现有护栏校验(模型目录/工具白名单/命名),入池并广播 |
| `finish_run` | status: succeeded\|failed, summary, artifacts[] | 声明整个 run 结束;引擎收尾 |

### 5.2 worker 的 MCP 工具子集

| 工具 | 条件 | 行为 |
|---|---|---|
| `report_progress` | 总是 | 中途结构化进度上报(写入 Task.Messages,coordinator 的 await 可见) |
| `ask_coordinator` | 总是 | 任务转 input-required,阻塞等 coordinator 答复(反向提问的协议化) |
| `handoff` | AllowPeerHandoff | 建子任务给指定 agent(CreatedBy=本任务,Depth+1,预算检查),coordinator 收到通知事件 |
| `ask_agent` | AllowPeerHandoff | 向血缘相邻任务(自己的上游/下游)发一条问答消息,同样入台账 |

工具注入方式:会话 cwd = agent home,在 home 写入 `.mcp.json` 指向 hub 的 HTTP MCP 端点,并注入一次性 token(`LOOM_TASK_TOKEN`)标识 run/task 身份。备选:ACP `session/new` 的 `mcpServers` 参数直接传(实现时验证 claude-code-acp 对该参数的支持度,择优)。

### 5.3 A2A 映射

| loom 概念 | A2A 概念 |
|---|---|
| 池 agent | A2A server(hub 托管),Agent Card 自动生成 |
| delegate | `message/send` 创建 Task |
| worker 完成信封 | Task → completed,信封内容作为 Artifact + 最终 Message |
| ask_coordinator | Task → `input-required`,await 方收到问题 |
| send_message | 对既有 Task 追加 message |
| 实时活动 | Task streaming(SSE)事件 |
| 取消 | `tasks/cancel` |

### 5.4 策略引擎(硬护栏,全部在 Go 侧)

- **计数**:任务总数 > MaxTasks → delegate/handoff 返回结构化错误(coordinator 能读懂并收敛)
- **深度**:Depth > MaxDelegationDepth → 拒绝 handoff
- **并发**:超过 MaxParallel 的任务排队(submitted 不启动会话)
- **超时**:单任务墙钟到 → 会话 cancel,Task failed(timeout);run 墙钟到 → 全体 cancel,run failed
- **轮次**:单任务消息轮次 > MaxTurnsPerTask → 拒绝 send_message,提示 coordinator 改为收敛决策
- **停滞检测**:全台账 N 分钟(默认 10)无状态跳变 → 事件告警 + 向 coordinator 注入一条系统提示(要求给出进度判断或收尾)
- **审批点**(ApprovalPolicy=initial):coordinator 首次 delegate 前必须先调 `propose_plan`(任务清单+新 agent 提案)→ run 转 awaiting_approval → UI 展示批准后放行。此后追加委派不再逐条审批(靠预算兜底)

### 5.5 成本核算(两种模式通用,可先于 v2 落地 = P0)

**口径**:ACP 走订阅、真实扣费为零,但一律按 **Claude API 牌价折算等价成本**并全链路标记——这是跨 workflow/agent 比较投入产出的统一量纲。UI 处标注「est.(API 等价)」以免误读为实际账单。

**采集(按执行路径)**

| 路径 | 来源 | 说明 |
|---|---|---|
| claude CLI(planner / claude backend) | `--output-format json` 的 `total_cost_usd` + `modelUsage` | 现成,已是 API 等价口径 |
| ACP(executor) | 会话 transcript `~/.claude/projects/<agent-home 编码路径>/<sessionId>.jsonl` | ACP `session/new` 返回的 sessionId 即 transcript 文件名(已实测),join 精确可靠。任务结束(及每轮 Prompt 后)解析:对每条 assistant 记录取 `message.model` + `message.usage`,**按 message.id 去重取末次**(流式会写多条同 id 部分记录),分模型累加 |
| mock | 恒为 0 | |

**定价表**:`model.ModelCatalog` 扩展 `InputPerMTok / OutputPerMTok`(Fable 5 $10/$50,Opus 5 与 Opus 4.8 $5/$25,Sonnet 5 与 4.6 $3/$15,Haiku 4.5 $1/$5);cache_write 计 1.25×input、cache_read 计 0.1×input。定价随目录维护,不散落。

```
cost = Σ_model ( input×P_in + output×P_out + cache_write×1.25×P_in + cache_read×0.1×P_in )
```

**归集层级与存储**

- `llm.Result` / `Session` 增加 `Usage TokenUsage`;节点/Task 记 `CostUSD + Usage`(NodeState.CostUSD 已存在,补 Usage 细分)
- Run 聚合不变(含 planner/coordinator 自身的成本,coordinator 会话同样按 transcript 核算)
- 新增**成本台账** `data/costs.jsonl`:每条 `{ts, workflow_id, run_id, node/task_id, agent, model, usage, cost_usd}` 追加写——这是 per-agent / per-workflow / per-model 聚合的唯一事实源,避免每次扫全部 run
- API:`GET /api/costs/summary?by=workflow|agent|model`;UI 展示:workflow 卡片累计成本、agent 卡片累计成本(该 agent 历史所有执行)、run 头部(已有)、节点/Task 抽屉(已有,补 token 细分)

---

## 6. 需要的重构

### 6.1 llm 层:从一次性调用到会话对象

```go
// 现有 Backend.Complete 保留(static 模式与 planner 继续用)
type SessionBackend interface {
    Open(ctx, req SessionRequest) (Session, error) // req: agent home/tools/model
}
type Session interface {
    Prompt(ctx, text string, onUpdate func(...)) (*Result, error) // 可多次调用
    Cancel(ctx) error
    Close() error
}
```

ACP 实现:每任务 spawn 一个 claude-code-acp 进程,`session/new` 一次,任务期间保活;`Prompt` 即 `session/prompt`。static 模式的 `Complete` 变为 Open+Prompt+Close 的薄包装,行为不变。

### 6.2 引擎:双驱动

- `engine.drive`(static)不动
- 新增 `engine.coordinate`(dynamic):启动 coordinator 会话(带 hub 工具),之后引擎只做:任务调度(队列/并发)、A2A 路由、策略执行、台账持久化+SSE。coordinator 会话结束而未调 finish_run → run failed("coordinator ended without verdict")

### 6.3 UI

- Workflow 编辑器:mode 单选;dynamic 显示 coordinator(模型/系统提示)与预算表单
- Run 页(dynamic):**任务树**取代 DAG——按血缘缩进的实时列表(状态徽章/agent/耗时/活动),点开抽屉看消息往来与产物;coordinator 有常驻置顶卡片(状态+最近决策);审批视图展示首批任务清单+新 agent 提案
- 事件类型新增:task_created / task_status / task_message / handoff / budget_hit / stall_warning

---

## 7. coordinator 系统提示要点(实现时精调)

- 职责:分解、委派、跟进、收敛、终审;**不自己干活**(它没有文件工具,只有 hub 工具)
- 委派纪律:指令必须自包含(worker 看不到你的上下文);产物走交换目录;优先复用池 agent,缺人才 create_agent
- 收敛纪律:await 回来先判断"是否推进了整体目标";同一任务两轮无实质进展必须换策略(换人/拆小/收窄);预算错误是收敛信号不是障碍
- 结束纪律:所有验收点满足即 finish_run,附最终产物清单;无法完成时 finish_run(failed) 并说明缺口

---

## 8. 交付物与验收

**新增/修改模块**

| 模块 | 内容 |
|---|---|
| `internal/llm`(改) | Session 接口 + ACP 会话实现;Complete 兼容包装 |
| `internal/hub`(新) | A2A gateway(a2a-go)、MCP server、策略引擎、任务台账 |
| `internal/engine`(改) | coordinate 驱动;static 路径零改动 |
| `internal/model`(改) | Workflow.Mode/Coordinator/Budget;Run.Tasks;Task |
| `internal/server`(改) | 台账 API、任务消息 API、SSE 扩展 |
| `web`(改) | mode 表单、任务树视图、消息抽屉 |
| 种子 | 新 workflow「动态编排」:mode=dynamic,coordinator=claude-opus-5,默认预算,approval=initial |

**验收场景**

1. **回归**:全部现有 static 测试与 E2E 不变绿→不合格
2. **单元**:预算护栏(计数/深度/并发/轮次/墙钟)全部有拒绝路径测试;Session 生命周期;台账状态机非法跳变拒绝
3. **E2E-1(委派回报)**:目标交给动态 workflow → coordinator 分解 2-3 任务并行委派 → worker 完成信封回报 → finish_run 成功;台账完整,UI 任务树实时
4. **E2E-2(反问)**:构造含歧义任务 → worker `ask_coordinator` → input-required → coordinator 答复 → 完成
5. **E2E-3(peer handoff)**:开 AllowPeerHandoff → A 完成后 handoff 给 B → 血缘与通知正确;关掉开关时 handoff 被拒
6. **E2E-4(预算)**:MaxTasks 设小 → coordinator 收到预算错误后收敛而非死循环;停滞检测触发告警
7. **E2E-5(成本)**:任一真实 ACP run 结束后,每个节点/Task 的 CostUSD > 0 且 Usage 细分完整;run 聚合 = Σ 节点 + planner/coordinator;`/api/costs/summary` 的 per-agent、per-workflow 汇总与台账一致;mock run 恒为 0

---

## 9. 实施阶段

| 阶段 | 内容 | 备注 |
|---|---|---|
| P0 | 成本核算(§5.5):定价表入 ModelCatalog、ACP transcript 解析、成本台账 + 汇总 API、workflow/agent 卡片累计成本 | **独立于 dynamic 模式,先在现有 v1 落地**,static/dynamic 通用 |
| P1 | llm.Session 重构 + hub(MCP 工具集/台账/策略,A2A 对象模型但进程内路由)+ coordinate 驱动 + 任务树 UI + E2E-1/4 | 核心价值先落地;A2A 语义完整,传输可先 loopback |
| P2 | a2a-go 真实端点与 Agent Card(hub 网关挂 HTTP),内部路由切到 A2A client;E2E-2 | 完成"协议正名",外部 A2A client 可调用池 agent |
| P3 | peer handoff / ask_agent + 停滞检测精调;E2E-3 | 策略最敏感的部分放最后 |

P1 结束即可日常使用;P2/P3 增量无破坏。

---

## 10. 风险与开放问题

1. **a2a-go SDK 成熟度**:实现前先验证其 server/client API 与任务状态机覆盖度;若关键能力缺失,P1 的进程内路由本就不依赖它,P2 再评估(原则:标准协议不手搓,但也不为凑协议阉割语义)
2. **MCP 注入通道**:`.mcp.json` in home vs ACP `mcpServers` 参数——实现时以 claude-code-acp 实测为准,取更稳的
3. **coordinator 上下文增长**:长 run 依赖 claude 自身 compaction;await 返回摘要而非全文,台账全文只进 UI/审计
4. **成本核算的依赖面**:transcript 解析依赖 claude 会话 jsonl 的路径规则与 `message.usage` 字段形态(非公开契约,CLI 升级可能变)——解析器要防御性容错,失败时成本记 0 并打 `cost_unavailable` 事件,不影响执行;定价表随官方牌价手工维护(Sonnet 5 介绍价 2026-08-31 结束后按 $3/$15 牌价)
5. **worker 会话保活的资源占用**:MaxParallel 兜底;空闲(排队)任务不占会话
6. **收敛质量**:护栏只能保证"不失控",收敛的聪明程度取决于 coordinator 提示词——预留 per-workflow SystemPrompt 迭代空间,必要时后续引入"进度评审员"只读 agent
