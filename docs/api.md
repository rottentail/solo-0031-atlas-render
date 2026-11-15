# HTTP 协议

所有业务路径以 `/v1` 开头。请求体使用 JSON，素材上传直接使用 PNG 字节。

| 方法和路径 | 请求 | 结果 |
|---|---|---|
| PUT /scenes/{id} | name, expectedRevision, document | 完整场景，200 |
| GET /scenes/{id} | 无 | 完整场景，200 |
| POST /scenes/{id}/state | expectedRevision, state | 完整场景，200 |
| POST /scenes/{id}/snapshots | expectedRevision | 快照和素材闭包，201 |
| PUT /assets/{id} | PNG字节 | 素材及base64字节，201 |
| POST /assets/{id}/retire | 无 | state，200 |
| GET /snapshots/{id} | 无 | 快照，200 |
| POST /snapshots/{id}/revoke | 无 | state，200 |
| GET /snapshots/{id}/package | 无 | atlas-scene交换包，200 |
| POST /imports/{id} | name, package | 新场景，201 |
| POST /jobs | snapshotId, scale；请求头Idempotency-Key | 任务，202 |
| GET /jobs/{id} | 无 | 任务，200 |
| POST /jobs/{id}/cancel | 无 | 任务，200 |
| POST /jobs/{id}/retry | 无 | 任务，202 |
| POST /batches | snapshotId, scales（1/2的去重数组）；请求头Idempotency-Key | 输出集合视图，202 |
| GET /batches/{id} | 无 | 集合状态和全部成员任务视图，200 |
| POST /batches/{id}/cancel | 无 | 集合视图，200 |
| POST /batches/{id}/retry | 无 | 集合视图，202 |
| GET /artifacts/{id} | 可选If-None-Match | PNG，200或304 |
| DELETE /artifacts/{id} | 无 | state，200 |
| POST /compare | left, right（结果ID） | 像素比较，200 |
| GET /status | 无 | 场景/素材/快照/任务计数和结果字节数 |
| GET /events | 无 | 最多500个有序领域事件 |

`GET /healthz` 不带 `/v1`，返回ready。它表明进程已打开并恢复状态，不包含单独的磁盘可写探测。任务响应省略固定输入的有效载荷，输入细节通过快照接口读取。

文档字段：width、height、background、layers。每个图层有id、kind、width、height、transform、opacity、fill；transform包括x、y、scaleX、scaleY、degrees。图层可以有clip（x/y/width/height），group使用children，polygon使用points，image使用assetId。solid颜料有color；linear还包括endColor、from、to。示例是可直接提交的完整请求。

幂等键长度1–80，使用英文字母、数字、下划线和连字符，首位不能是后两者。它在服务实例持久状态中全局生效，不随任务完成、取消、结果清理或进程重启失效。改换请求语义需使用新键。

## 多尺寸输出集合

需要同一快照的多个尺寸时，客户端应一次提交 `POST /v1/batches`，由服务端在单个持久事务中承诺整组：集合记录和全部成员任务要么同时落地，要么一个都不写入。集合只接受 `scales` 为1或2的非空去重数组；重复提交使用相同幂等键返回原集合，不会多出成员任务；同键改换快照或尺寸语义返回409，单任务收据与集合收据互不复用。预算不足（任一尺寸编译超过四千万图层像素）或队列容量不足（成员数加未结束任务超过64）在写入前判定，失败返回429且不留下半个集合。

集合状态由成员任务的已提交状态推导，可随时查询：`active` 表示仍有成员排队或运行，`settled` 表示全部到达终态但至少一个失败或取消，`completed` 表示全部成功。集合取消在一个事务中取消所有尚未终态的成员，已成功成员的结果保持不变；渲染中的成员收到中断信号，提交前再次核对状态和尝试身份，过期结果不会覆盖已提交的取消。集合重试只重新排队失败或取消的成员并递增其attempt，成功成员不参与；无可重试成员返回409。成员任务仍是普通任务，可通过 `/v1/jobs/{id}` 单尺寸接口查询、取消和重试，集合状态在同一事务内同步。进程重启后运行中的成员回到排队，集合状态从成员事实重新推导。

服务接受相同内容的素材重复上传，退役素材不接受重新上传。交换包导出保留快照状态；导入会创建全新的active场景，原快照不作为目标服务中的快照记录落地。导入目标ID必须未使用；共享素材必须内容相同且仍可用。
