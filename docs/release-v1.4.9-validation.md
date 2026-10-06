# V1.4.9 验证范围 / Validation scope

业务源码部署及用户验收日期：2026-10-06。发布准备沿用已验收的262个Go输入，未修改运行逻辑；文档和打包补充在发布前单独核对。

## 已有证据

- 合并修订普通回归：同源码分组656个顶层入口、2026个唯一测试节点，0测试失败、0测试跳过；无测试文件包单独记录。
- 有限race：4个入口、7个节点，覆盖并发归并、补充版本、别名生命周期与状态重启。没有全量race通过结论。
- Hills联合候选专项：83个入口、432个节点通过，涵盖Counts、任务六、语言参数和授权/错误校验。
- 实际Hills请求与用户验收完成；真实剧集列表中同一S1E1合为一个条目，已响应上游的多版本完整归组。一次上游超时样本不作为全部来源版本均已返回的证据。
- 用户确认合并修订客户端验收完成。既有分页和未覆盖恢复边界不因用户验收声明扩大为穷尽验证。

## 发布环境

- Go1.23.12：同一组262个Go输入，完整入口清单对账658个顶层测试、2066个唯一测试节点，最终覆盖集合0失败、0跳过。此前全量命令在15分钟包预算处超时；保留其已完整通过的468个入口，另一次独立运行通过剩余190个入口，未将中断入口记为通过。
- 首次只读工作区验证因测试需要写入临时存储而终止；后续均使用隔离的可写源码副本。初始环境失败和超时日志保留，不写成整套单次通过。
- Go1.23 vet、amd64静态版本构建及`--version`检查通过；业务Go输入与用户已验收部署源码逐文件一致。
- 44项面板契约通过；样式重建无差异；三个安装/管理脚本语法、Git差异格式和Actions配置检查通过。
- CI与Release继续运行普通全量回归，包预算为45分钟。完整race仍为手动可选项，本轮未执行。
- 开发候选Go1.27.1动态构建单独保留，不能用其二进制哈希指代六架构静态Release。

## 持续保留的边界

部分列表每源最多5000原始候选；ParentId按上游原始条目分页。不做三来源间接冲突审计或时间轴换算。Counts逐源累加官方三个字段，没有第二组去重/版本数指标。CapyPlayer统计503专项暂缓。没有全量race、穷尽的恢复断点或所有客户端行为覆盖结论。

## English summary

The business source was deployed and accepted on 2026-10-06. Historical ordinary regression, limited race, counts compatibility and isolated HTTP/storage tests establish the stated scope. User acceptance does not expand coverage to untested pagination, recovery or every client.

Go1.23.12 checks account for all 658 top-level tests and 2066 unique test nodes across disjoint completed groups. An initial read-only workspace failure and a later 15-minute timeout were retained as unsuccessful attempts; incomplete roots were rerun in full. Vet, the versioned static amd64 build, 44 panel contracts, stylesheet regeneration, script syntax and Actions configuration checks passed. Ordinary CI and Release tests use a 45-minute package budget. The dynamic development candidate is retained separately. Full race was not run. Counts remain three per-upstream sums; candidate caps, raw ParentId paging and deferred CapyPlayer counts work remain unchanged.
