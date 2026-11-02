# S6b：持久化恢复、原生适配器与 Release/pin

> 本文是源码实现与验证边界，不是主机部署、Release 发布、pin 安装或生产批准。

## 范围

S6b 在一个线性分支中补齐 14 个内部施工切片：

1. 边界重现；2. owner-only event journal；3. 阶段 intent/observation；4. durable activation；5. unknown start fencing；6. per-object restore intent；7. launchd capability seam；8. systemd `--system/--user` contract；9. metadata/Trash helper gate；10. epoch suspension/reactivation；11. exact-source/Docker 四戳 buildinfo；12. evidence-bounded pin emitter；13. sequential CI inventory；14. 文档与交叉引用。

切片不是审查或上线节点。每片拥有独立提交和本地红绿测试；公共回归入口是 [`scripts/upgrade-regression.sh`](../../scripts/upgrade-regression.sh)，清单是 [`scripts/regression_gate.list`](../../scripts/regression_gate.list)。

## 持久化安全边界

[`scripts/upgrade_state.py`](../../scripts/upgrade_state.py) 是 S6b 新增控制日志的唯一写入口。事件采用 `ags.upgrade-event.v1`，owner-only 目录、固定序号、前一事件摘要、canonical JSON、O_NOFOLLOW、fsync 文件和目录。已发布 event 才是提交点；staging 文件不产生权限。`current.json` 是可重建投影，不是第二授权源；链洞、重复字段、坏摘要、symlink、owner/mode/hardlink 异常均阻断。

`commit_phase` 只写 intent 或 observation，不执行外部效果。效果必须先有 intent，之后以相同 effect id 由独立读回关闭。`recover_local` 只修投影、忽略未发布 staging，不调用 manager、网络、服务或业务数据库。S3/S4 的既有事实格式和拥有者保持不变。

## 生命周期与恢复

`activate_durable` 独立校验 lifecycle approval，先写 `authority_activated`，重试返回同一投影。`old_start_durable` 先写 spawn intent；只有同时具有 pid、boot identity 和 started observation 才写 `start_observed`，未知结果写 `start_blocked`，不重发 token。`suspend_durable` 后，第二个 epoch 必须使用严格更新的 epoch 和新的批准。恢复对象切换使用 [`restore_object_durable`](../../scripts/upgrade_full_restore.py)：先写每对象 intent，rename 后 hash readback；结果未知保持 blocked，不在 old/new 间猜选。

这些接口不授予默认启动、apply、恢复或服务重启权。默认 policy 为空，公开 apply 仍为 83，直接 v2 install 仍为 83，离线 preparation 仍 `installable=false`。

## 原生适配器

launchd 与 systemd 工厂只接受固定 profile 和显式注入 runner；不从请求读取任意 argv，不在测试中调用真实 launchctl/systemctl。systemd 命令使用 `systemctl --system` 或 `systemctl --user`，不可混 scope。能力在真实隔离 probe 前标记 `native_verified=false`。缺少 provisioned metadata helper 或 Trash mover 时在 claim 前拒绝；fixture readback 不是生产证据。

本机未验证 Darwin launchd、Linux systemd/D-Bus/cgroup、ACL/xattr/fsync、root helper、native Trash、隔离 VM、生产副本、掉电、正常 reboot 或 imile 实机行为。这些是 `UNVERIFIED`，不是 skip pass。

## Buildinfo 与 pin emitter

exact-source binary 和 Docker 构建统一写入：

- `server.gitSHA=R`
- `internal/buildinfo.Version=source-R`（Release builder 的 V 仍由既有 builder 写入）
- `internal/buildinfo.Revision=R`
- `internal/buildinfo.Tree=T`

Docker 的 Alpine/CGO-disabled ABI 仍不等于原生 Release 证据。

`upgrade_pin.emit_verified` 只在调用者提供两平台、tag-push、hosted-runner、`release-control` provenance 时继续，并复用现有 preparation 校验。它不调用 GitHub、cosign、manager，不安装 pin，不发 Release，不打 tag；返回的 `ags.deploy-release-pin.v2` 是 `installable=false` 的源码准备 envelope。dispatch 证书、缺平台、非 push、重复或过期证据必须拒绝。真实 immutable Release、九资产、签名、tag-push、control approval 和生产 pin 仍需另批。

## 验证与交叉引用

- 生命周期、恢复和 cold window：[`operations/local-upgrade.md`](../operations/local-upgrade.md)
- 全组与对象恢复：[`operations/backup-recovery.md`](../operations/backup-recovery.md)
- Release、buildinfo、pin：[`operations/releases.md`](../operations/releases.md)
- helper、bundle、provisioning：[`operations/upgrade-provisioning.md`](../operations/upgrade-provisioning.md)
- CI 与 UNVERIFIED 边界：[`ci.md`](../ci.md)
- launcher/runtime fence：[`operations/user-runtime.md`](../operations/user-runtime.md)
- fork 生成与发布门禁：[`../../fork/README.md`](../../fork/README.md)

本 PR 不改 `fork/release-source.json`，不选择 V，不发 Release，不安装 pin，不部署，不修改 GitHub settings/variables/rulesets，不合并。后续发布 control identity、tag、Release、真实 pin 和部署窗口均另批。
