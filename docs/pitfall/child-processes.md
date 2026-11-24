# 子进程（`internal/collie/sysmon_darwin.go`、`updates.go`）

## collie 怎么结束都不能留下子进程

部署和开发实例会频繁重启 collie，也常被 `kill -9`，所以子进程不能指望 collie 退出时替它们收尾，得自己能退出：

- **常驻程序靠管道退出**。macOS 采样常驻的 `iostat`、`vm_stat` 每 10 秒写一行。collie 不在了，下一次写管道就收到 SIGPIPE 并退出。Go 只把启动时就被忽略的 SIGHUP、SIGINT 原样留给子进程，SIGPIPE 在子进程里总是默认行为。
- **不要用终端（`script`、pty）包常驻程序**。终端断开时只发 SIGHUP。用 `nohup` 或在已忽略 SIGHUP 的环境里启动的 collie 被杀后，nettop 收不到 SIGHUP，父进程变成 launchd，在已断开的终端上空转，每个占约 1.4 核。2026-11-25 macpro 上积累了 16 个这样的进程，load 到 413，node 和 hub 断连 25 分钟（01:32–01:57），杀掉这些进程后 node 立刻重连。即使在正常运行时，`script` 下的 nettop 也一直占约 80% 的核。现在每次采样跑一次 `nettop -L 1`，耗时约 0.09 秒，网络速率用两次累计值相减得出。启动时 `endStaleNettops` 会结束旧版本留下的这类进程：父进程是 launchd，命令行是旧版本的那组参数。
- **自更新 exec 之后要回收旧子进程**。`syscall.Exec` 换成新版本后进程 ID 不变，旧版本启动的子进程还是它的子进程，但新代码不知道它们，结束后就成了僵尸。`execUpdated` 会带上 `COLLIE_UPDATED`，新进程启动时第一步运行 `reapInherited`，逐个 `wait4` 这些子进程。

验证（`sysmon_darwin_test.go`、`updates_test.go`）：`TestSysmonKilledLeavesNothing` 在忽略 SIGHUP 的子进程里启动采样，`kill -9` 之后确认 10 秒内没有子进程残留；`TestReapInherited` 在 exec 前留一个子进程，确认 exec 后它被回收，没有变成僵尸。
