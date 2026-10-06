# 媒体合并规则 / Media merge rules

适用版本：V1.6.0。合并在浏览、搜索、季集列表和详情等请求遇到候选时进行；启动或创建用户不扫描所有上游的全库。

| 对象 | 判定依据 |
| --- | --- |
| 电影、整部剧 | 同类型，比较同一命名空间的 TMDB / IMDb / TVDB ID |
| 季 | 已确认同一父剧，且明确季号相同 |
| 单集 | 已确认同一父剧，且明确季号、集号相同，例如 S1E1 |
| 没有可直接比较的有效作品 ID | 完整名称＋年份；英文 ASCII 字母转小写后精确匹配 |

共有 ID 只要有一个冲突，就拒绝建立新合并关系，即使另一个 ID 相同。不进行译名、标点或空格模糊匹配、联网 ID 映射。必要名称、年份或编号缺失时保持独立。明确 S0 有效，集号必须是正整数。单集标题、年份不代替父剧身份，共有单集 ID 冲突仍会拒绝新关系。

时长不参与合并；时长不同、未知、为零或不可信均不会独立阻止同一作品归组。每个版本保留自身时长、画质、编码与音轨，不进行内容指纹或时间轴换算。

## 版本保留与选择

版本通过 `(server_id, original ItemID, MediaSourceID)` 定位。相同上游的不同条目、同一条目的多个版本，与跨上游版本使用相同身份规则：A有8个版本、B有2个版本，身份一致时保留10个来源；身份冲突时分别保留8和2个版本。不同定位不会因为名称、画质或时长相同而删除。

重复返回相同定位幂等；部分响应只增加已知版本，不删除历史成员。缺媒体源时可保存身份证明，但不会生成虚构播放路由。列表、详情和PlaybackInfo只展示当前授权、实际响应中的可用来源；上游暂时不可用时，不代表其持久化版本关系被删除。选择具体版本后播放路由仍精确指向该版本。

## 旧链接与观看状态

已保存关系不会因元数据变化主动拆分；请求遇到且证明同一作品的旧拆分组可归并。旧Virtual ID保留为别名，历史证明及观看记录保留。

同一普通用户在同组合并版本间共享已观看、收藏及原始续播位置；不同用户、不同身份保持隔离。不同剪辑也共享这些状态，不增加位置换算。

## 既有边界

部分聚合/本地筛选路径每上游每请求最多获取5000个原始候选，不是5000个去重作品或版本。`ParentId`路径仍按上游原始条目分页；不新增全库索引、全局去重分页或三来源间接冲突审计。媒体库统计继续逐源累计官方Counts，口径与合并列表不同。

## English summary

V1.6.0 merges on demand. Movies and series compare valid TMDB, IMDb or TVDB identifiers in the same namespace; any shared-provider conflict blocks a new merge. If no valid identifier can be directly compared, complete name and year are required, with ASCII case folding only. Seasons and episodes require a proven shared parent series and valid matching numbers.

Runtime never gates identity. All distinct server/item/source locators are retained, including multiple versions from one upstream. Partial responses add rather than prune known members. Old virtual IDs remain aliases when proven groups coalesce. One regular user's merged group shares played state, favorites and the raw resume position.

Source visibility still depends on current authorization and actual upstream responses. Explicit selection routes to the chosen version. The existing 5000-candidate cap and upstream raw-item paging remain; no full-library scan or timeline conversion is added.
