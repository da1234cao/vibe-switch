# vibe-switch — 测试驱动设计 (TDD)

本项目从零用 Go 实现一个二层交换机。**第一步先建立一套可复用的行为测试基准**：
用 Linux bridge 作为参考交换机 (test oracle) 把测试跑通，将来把"被测交换机"换成
Go 实现，跑**同一套**测试来驱动开发。

## 设计原则

1. **只断言可观测行为**——连通性、收到/收不到帧、tag 有无、是否泛洪。绝不解析交换机
   内部状态（FDB dump、日志）。因此测试对实现零约束。
2. **学习等内部行为用行为间接验证**——例如"已知单播不泛洪"证明 MAC 学习生效。
3. **被测交换机可插拔**——`SWITCH` 环境变量选择实现；测试体不变。
4. **接入契约最小化**——拓扑只交给交换机一组 Linux 接口，契约是"在这些接口间做二层
   转发"。I/O 机制（AF_PACKET / XDP / TAP …）不在契约内。

## 拓扑

```
            netns swns  (交换机 + 端口侧)
        ┌───────────────────────────────┐
        │      br0 (或将来的 Go 交换机)   │
        │   sw1   sw2   sw3  ...  swN     │
        └────┬─────┬─────┬────────┬───────┘
         veth│ veth│ veth│    veth│
        ┌────┴┐ ┌──┴─┐ ┌─┴──┐  ┌──┴─┐
        │ h1  │ │ h2 │ │ h3 │  │ hN │     每个 host 一个 netns
        │eth0 │ │eth0│ │eth0│  │eth0│     分配 MAC/IP
        └─────┘ └────┘ └────┘  └────┘
```

- 每个端口 = 一对 veth：主机侧在 `h{i}` netns 内命名 `eth0`；交换机侧 `sw{i}` 留在 `swns`。
- 参考实现把 `sw{i}` enslave 进 `br0`；VLAN 用例额外开 `vlan_filtering`、配 per-port VLAN。
- **拓扑卫生**：所有接口 up；关闭 IPv6（去除 MLD/DAD 噪声、避免老化用例被动重学）；
  抓包接口设混杂。

## 测试矩阵（对 Linux bridge 全部通过）

| # | 用例 | 断言 |
|---|------|------|
| 1 | 连通性 | h1 ping h2 成功 |
| 2 | 已知单播不泛洪 | 学习 h2 后，h1→h2 仅 h2 收到，h3/h4 不收到 |
| 3 | 未知单播泛洪 | h1→未知 MAC，h2/h3/h4 都收到 |
| 4 | 广播泛洪 | h1→ff:ff:..，所有其他口收到 |
| 5 | 入端口不回灌 | h1 发出的帧不从入口返回（抓包排除 `PACKET_OUTGOING`）|
| 6 | MAC 老化 | 缩短 ageing_time，静默超时后已知单播重新泛洪 |
| 7 | 同 VLAN 连通 | h1(v10) ping h2(v10) 成功 |
| 8 | 跨 VLAN 隔离 | h1(v10) ping h3(v20) 失败 |
| 9 | access 去标签 | access 口出帧 untagged |
| 10 | trunk 透传 | trunk 入帧(vid10)：另一 trunk 保留 tag；access 剥 tag；v20 收不到 |
| 11 | PVID 归类 | access 口 untagged 帧只到达同 VLAN 成员 |

VLAN 拓扑（5 口）：h1/h2 = access v10，h3 = access v20，h4/h5 = trunk(10,20)。

### 性能（默认只报告，不设硬阈值）

- **吞吐**：帧长 {64,512,1500} 各打流，报告 pps / Mbit/s / 丢包。
- **时延**：低速带时间戳流，报告 min/p50/p99/max（单向，单调时钟同源）。
- **压力丢包**：多档目标速率的丢包率。

> 软件 veth 环境无真实网卡，数字意义在于**相对比较**（Go 交换机 vs Linux bridge 同机），
> 非绝对硬件指标。

可选回归门禁（设了才断言）：`PERF_MIN_PPS` / `PERF_MAX_LOSS` / `PERF_MAX_P99_US`。

### Linux bridge 基准（本机参考，仅供量级参考）

| 项 | 数值 |
|----|------|
| 吞吐 64B | ~180k pps (~92 Mbit/s) |
| 吞吐 1500B | ~160k pps (~1.9 Gbit/s) |
| 时延 | min ~35µs / p50 ~210µs / p99 ~2.1ms |
| 压力 10k–100k pps | 0% 丢包 |

## 运行

```bash
./run_tests.sh                         # 全部，Linux bridge（需要 root，会自动 sudo）
SWITCH=bridge go test ./test -v        # 等价（已是 root 时）
sudo -E go test ./test -run TestVLAN -v
PERF_MIN_PPS=50000 ./run_tests.sh -run TestPerf   # 带回归门禁
```

要求：root（netns/veth/AF_PACKET）、Go、`iproute2`、`bridge-utils`。

## 代码结构

```
internal/harness/   测试台架（不含任何交换机实现）
  topology.go       netns/veth 拓扑搭建与拆除
  switch.go         Switch 接口 + BridgeSwitch + NewSwitchUnderTest + AgeingConfigurable
  probe.go          openInNetns + afpacket 收发 + gopacket 造/解包 + Capture
  perf.go           Sender/Counter + 吞吐/时延测量
test/               行为测试（package test）
  main_test.go      TestMain（root 检查）+ 公共 helper
  l2_test.go        用例 1–6
  vlan_test.go      用例 7–11
  perf_test.go      性能
```

依赖：`gopacket/gopacket`（+ `afpacket`、`layers`）、`vishvananda/netns`、`golang.org/x/net/bpf`。

## 将来接入 Go 交换机

1. 自由实现交换机（结构/I/O/并发不受限）。
2. 在 `internal/harness/switch.go` 加一个薄 `GoSwitch` 适配器，实现 `Switch`（`Start`/`Stop`/`Name`）；
   可选实现 `AgeingConfigurable` 让老化用例生效。
3. `NewSwitchUnderTest` 增加 `case "goswitch"`。
4. `SWITCH=goswitch ./run_tests.sh` —— 跑同一套测试，无需改动任何测试体。
