# HTTP 协议

所有业务路径以 `/v1` 开头。请求体使用 JSON，素材上传直接使用 PNG 字节。

| 方法和路径 | 请求 | 结果 |
|---|---|---|
| PUT /scenes/{id} | name, expectedRevision, document | 完整场景，200 |
| PATCH /scenes/{id} | expectedRevision, name?, layers?, deleteLayers?, order? | 场景编辑结果，200 |
| GET /scenes/{id} | 无 | 完整场景，200 |
| POST /scenes/{id}/state | expectedRevision, state | 完整场景，200 |
| POST /scenes/{id}/snapshots | expectedRevision，可选edit | 快照和素材闭包，201 |
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
| GET /artifacts/{id} | 可选If-None-Match | PNG，200或304 |
| DELETE /artifacts/{id} | 无 | state，200 |
| POST /compare | left, right（结果ID） | 像素比较，200 |
| GET /status | 无 | 场景/素材/快照/任务计数和结果字节数 |
| GET /events | 无 | 最多500个有序领域事件 |

`GET /healthz` 不带 `/v1`，返回ready。它表明进程已打开并恢复状态，不包含单独的磁盘可写探测。任务响应省略固定输入的有效载荷，输入细节通过快照接口读取。

文档字段：width、height、background、layers。每个图层有id、kind、width、height、transform、opacity、fill；transform包括x、y、scaleX、scaleY、degrees。图层可以有clip（x/y/width/height），group使用children，polygon使用points，image使用assetId。solid颜料有color；linear还包括endColor、from、to。示例是可直接提交的完整请求。

## 按图层修订

`PUT /scenes/{id}` 仍然整份替换文档，`expectedRevision` 不匹配时返回409，不读取也不合并任何内容。

`PATCH /scenes/{id}` 只提交自己改过的部分：

```json
{"expectedRevision": 3,
 "name": "新名称",
 "layers": [{"layer": {"id": "accent", "kind": "ellipse", "...": "完整图层"}, "parentId": ""}],
 "deleteLayers": ["old-badge"],
 "order": [{"parentId": "", "layerIds": ["plate", "accent", "inset"]}]}
```

服务把编辑合并到当前修订：与 `expectedRevision` 对应文档相比没有变化的图层保留已提交内容，删除、重排、移动也只作用于自己声明的部分。`layers` 是存在即替换、不存在即新增；新增图层未给 `parentId` 时追加到文档根，给 `parentId` 时追加到该组末尾。组的本地字段（变换、透明度、裁剪）可以单独更新，子图层必须通过各自的 `layers`、`deleteLayers`、`order` 条目修改，组内的旧 `children` 快照不会覆盖他人已提交的子图层。`order` 必须列出该父级的全部既有子图层（可含同一编辑新增的图层），其他编辑在此期间新增的子图层排在末尾。重复提交同一编辑是幂等的：文档没有净变化时不递增修订。

无法合并的修改返回409，`details.conflicts` 逐项给出 `layer`、`parent`、`reason` 与 `field`。reason 包括 `layer-modified`（同图层内容或变换被他人改动）、`layer-deleted`、`layer-created`、`layer-moved`、`layer-children-modified`、`order-modified` 和 `parent-deleted`；`details.expectedRevision` 与 `details.currentRevision` 说明合并的基线。冲突时不写入文档，客户端应重新读取场景后重新提交。编辑基线超出保留的32个修订时返回 `base-unavailable`，必须重新读取。

`POST /scenes/{id}/snapshots` 可以带 `edit`（结构同 `PATCH`），服务在同一次持久提交里先合并编辑再冻结结果：快照的 `revision` 与 `document` 等于该事务提交后的场景，不会混合两次修订。`expectedRevision` 必须是调用者实际读到的当前修订；过期时返回409，即使 `edit.expectedRevision` 更旧。冲突时既不生成快照也不写入半成品。没有 `edit` 的冻结仍要求 `expectedRevision` 等于当前修订。

幂等键长度1–80，使用英文字母、数字、下划线和连字符，首位不能是后两者。它在服务实例持久状态中全局生效，不随任务完成、取消、结果清理或进程重启失效。改换请求语义需使用新键。

服务接受相同内容的素材重复上传，退役素材不接受重新上传。交换包导出保留快照状态；导入会创建全新的active场景，原快照不作为目标服务中的快照记录落地。导入目标ID必须未使用；共享素材必须内容相同且仍可用。
