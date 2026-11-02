# S6b：持久化恢复、原生适配器与 Release/pin

> 本文是源码实现与验证边界，不是主机部署、Release 发布、pin 安装或生产批准。

## 范围

S6b 在一个线性分支中补齐 14 个内部施工切片：

1. 边界重现；2. owner-only event journal；3. 阶段 intent/observation；4. durable activation；5. unknown start fencing；6. per-object restore intent；7. launchd capability seam；8. systemd `--system/--user` contract；9. metadata/Trash helper gate；10. epoch suspension/reactivation；11. exact-source/Docker 四戳 buildinfo；12. evidence-bounded pin emitter；13. sequential CI inventory；14. 文档与交叉引用。

切片不是审查或上线节点。每片拥有独立提交和本地红绿测试；公共回归入口是 [`scripts/upgrade-regression.sh`](../../scripts/upgrade-regression.sh)，清单是 [`scripts/regression_gate.list`](../../scripts/regression_gate.list)。

## 持久化安全边界

[`scripts/upgrade_state.py`](../../scripts/upgrade_state.py) 是 S6b 新增控制日志的唯一写入口。事件采用 `ags.upgrade-event.v1`，owner-only 目录、固定序号、前一事件摘要、canonical JSON、O_NOFOLLOW、fsync 文件和目录。已发布 event 才是提交点；staging 文件不产生权限。`current.json` 是可重建投影，不是第二授权源；链洞、重复字段、坏摘要、symlink、owner/mode/hardlink 异常均阻断。

`commit_phase` 只写 intent 或 observation，不执行外部效果。效果必须先有 intent，之后以相同 effect id 由独立读回关闭。`recover_local` 只修投影、忽略未发布 staging，不调用 manager、网络、服务或业务数据库。S3/S4 的既有事实格式和拥有者保持不变。

S6b-fix B4 恢复了 journal header 的 host binding：`format.json` 的 `host_instance_sha256` 必须等于 `host_instance` 的摘要，且首个 event 的 host 必须与 header host 一致；不一致拒绝，错误码 `state_host_mismatch`。这是源码校验，不是主机身份现场验收。

## 生命周期与恢复

`activate_durable` 独立校验 lifecycle approval，先写 `authority_activated`，重试返回同一投影。`old_start_durable` 先写 spawn intent；只有同时具有 pid、boot identity 和 started observation 才写 `start_observed`，未知结果写 `start_blocked`，不重发 token。`suspend_durable` 后，第二个 epoch 必须使用严格更新的 epoch 和新的批准。恢复对象切换使用 [`restore_object_durable`](../../scripts/upgrade_full_restore.py)：先写每对象 intent，rename 后 hash readback；结果未知保持 blocked，不在 old/new 间猜选。

S6b-fix B1 要求 `target` 与 `source` 都位于经批准的 `restore_root` 内，拒绝 symlink 穿越、`..` 逃逸、硬链接和仓外路径。approval、binding 与 transaction 缺失即拒绝，不用零摘要或调用者的 expected_* 自证；调用者不能把 restore_root 指到部署根外。staging 文件与目标父目录在切换后 fsync。`fail_after='rename'` 只模拟未知观察，不是掉电语义证据。固定离线工具、锁与完整组验证路径本轮未接线。

S6b-fix B2 把 durable seam 以迁移期 shim 接入现有生产写者，而不是替换 S3/S4 事实格式：

- 已接线：`upgrade_engine.checkpoint` 按 `root/state` 或 `root/state/ags-autodeploy/local-v2` 定位部署根，并以 transaction 与 binding 区分同阶段冲突。`ColdWindow` 阶段、S4 completion 重试、`launcher_spawn`、`supervise` 的 spawn/exit 和 `Effects.stop` 前的 `suspend_durable` 都写 durable event。`startup_permitted` 在新日志存在但 format 缺失、authority 为空或仍有 spawn intent 时拒绝。`old_start_durable` 重试读回同一 generation。
- 未接线或 UNVERIFIED：没有跨进程重入，也没有真实两次升级链、主机恢复、服务重启或掉电。默认 policy 仍为空，公开 apply 仍退出 83，直接 install 仍退出 83。

这些接口不授予默认启动、apply、恢复或服务重启权。默认 policy 为空，公开 apply 仍为 83，直接 v2 install 仍为 83，离线 preparation 仍 `installable=false`。

## 原生适配器

launchd 与 systemd 工厂只接受固定 profile 和显式注入 runner；不从请求读取任意 argv。systemd 命令使用 `systemctl --system` 或 `systemctl --user`，不可混 scope。能力在真实隔离 probe 前标记 `native_verified=false`。缺少 provisioned metadata helper 或 Trash mover 时在 claim 前拒绝；fixture readback 不是生产证据。

S6b-fix B3 增加固定命令、一次性 label/unit 名、超时、输出上限和环境清洗的 runner，以及 launchd bootstrap/print/bootout、systemd enable/start/status/stop/disable 的 probe 序列。factory 返回的 Manager 接受无参数 suppress_restart/stop/start，并用注入 runner 读回；systemd factory 把 runner 注入 Manager。调用者传入的 facts 被拒绝。默认关闭。本仓库测试只用注入 runner，不调用真实 launchctl/systemctl。真实 OS probe 仍是 UNVERIFIED。

S6b-fix3 B6 把 Native adapter isolation 从 Fork CI 移到 [`.github/workflows/native-adapter.yml`](../../.github/workflows/native-adapter.yml)。它不属于 release required evidence：`fork/scripts/release.py` 与 `scripts/upgrade_host.py` 仍要求审查过的 12 个 Fork CI job 成功。清单外的 skipped job 不计入成功，也不使 inventory 失败。该 workflow 仅在 `workflow_dispatch` 且 `native_probe=true` 时运行注入序列，默认关闭，不使用私有 runner 或 secret，也不调用 launchctl/systemctl。未运行或 skipped 都是 UNVERIFIED，不是 pass。

已验证：注入 runner 下的序列、顺序、清理和失败回滚。UNVERIFIED：真实 Darwin launchd、Linux systemd/D-Bus/cgroup、ACL/xattr、root helper、native Trash、隔离 VM、生产副本、掉电、正常 reboot 或 imile 实机行为。这些不是 skip pass。

## Buildinfo 与 pin emitter

exact-source binary 和 Docker 构建统一写入：

- `server.gitSHA=R`
- `internal/buildinfo.Version=source-R`（Release builder 的 V 仍由既有 builder 写入）
- `internal/buildinfo.Revision=R`
- `internal/buildinfo.Tree=T`

Docker 的 Alpine/CGO-disabled ABI 仍不等于原生 Release 证据。

`upgrade_pin.emit_verified` 校验两平台 evidence：digest 必须等于候选资产 digest，必须有 run id 与 attempt，并把签名 bytes 交给注入的 verifier。伪造 digest 或 verifier 拒绝时返回 `release_attestation_unverified`。它不调用真实 GitHub、cosign 或发布 Release。返回的 `ags.deploy-release-pin.v2` 固定 `installable=false`。immutable API readback 与原子 receipt 发布本轮未做，仍是 UNVERIFIED。

## 验证与交叉引用

- 生命周期、恢复和 cold window：[`operations/local-upgrade.md`](../operations/local-upgrade.md)
- 全组与对象恢复：[`operations/backup-recovery.md`](../operations/backup-recovery.md)
- Release、buildinfo、pin：[`operations/releases.md`](../operations/releases.md)
- helper、bundle、provisioning：[`operations/upgrade-provisioning.md`](../operations/upgrade-provisioning.md)
- CI 与 UNVERIFIED 边界：[`ci.md`](../ci.md)
- launcher/runtime fence：[`operations/user-runtime.md`](../operations/user-runtime.md)
- fork 生成与发布门禁：[`../../fork/README.md`](../../fork/README.md)

本 PR 不改 `fork/release-source.json`，不选择 V，不发 Release，不安装 pin，不部署，不修改 GitHub settings/variables/rulesets，不合并。后续发布 control identity、tag、Release、真实 pin 和部署窗口均另批。
