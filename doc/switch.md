# vibe-switch — Go 软件交换机实现

用纯 Go 实现的用户态二层/VLAN 学习交换机。一套转发引擎，两个前端：

1. **测试前端** `SWITCH=goswitch`：接入 [TDD.md](TDD.md) 的行为测试台架，跑通同一套用例 1-11；
2. **生产前端** `cmd/vibe-switch`：独立二进制，在真实主机网卡上转发，附带 `ctl` 管理接口。

转发逻辑只此一份（`internal/goswitch.Engine`），测试通过即等于二进制行为通过。

## 代码结构

```
internal/goswitch/      转发引擎（不依赖 netns/harness）
  engine.go    生命周期、每端口 rxLoop、forward 决策、SetAgeing、gcLoop
  fdb.go       按 (vid,mac) 学习的转发表，RWMutex + atomic 时间戳，惰性老化
  vlan.go      端口角色、入口 VID 分类、出口 tag 增删（gopacket SerializeLayers）
  afpacket.go  PacketIO 接口、AF_PACKET 选项/InboundBPF/OpenInterface、rxLoop
  stats.go     每端口 atomic 计数器 + 只读状态快照
internal/control/       net/rpc over Unix socket 的管理接口（server + client）
cmd/vibe-switch/        独立二进制：默认跑交换机；ctl 子命令查状态
internal/harness/switch.go  GoSwitch 适配器（测试前端），NewSwitchUnderTest 的 goswitch 分支
```

## 转发模型

- 每端口一个 AF_PACKET 句柄 + 一个收包 goroutine；收到帧后同步完成"学习 → 查表 → 逐出口写"。
- 帧解析/重组一律用 gopacket：热路径用 `DecodingLayerParser`（零分配，每 goroutine 一个 parser）。
- **MAC 学习**：源 MAC（非组播）→ (vid, ingress) 写入 FDB；热路径同端口仅 atomic 刷新时间戳。
- **转发**：已知单播单发目标口（入端口不回灌）；广播/组播/未知单播泛洪到同 VLAN 其他成员。
- **VLAN**：access 口按 PVID 归类 untagged 帧、出口剥 tag；trunk 口按 802.1Q tag 归类、出口保留/补 tag；
  FDB 按 (vid,mac) 隔离 → 跨 VLAN 隔离。整机要么全 plain、要么全 VLAN-aware（不混配）。
- **老化**：FDB 表项带时间戳，`Lookup` 惰性过期 + 后台 gcLoop 删除；`-ageing` / `SetAgeing` 可调。
- **关停时序**：`close(done)` → 等 rxLoop 退出 → 再关句柄（关句柄会 unmap afpacket ring，先等读完毕避免段错误）。

## 运行二进制

需 root（AF_PACKET）。只有显式列出的网卡成为端口，其余不受影响。

```bash
make build-bin                                  # 编译 ./bin/vibe-switch

# 纯 L2：列出要用的网卡（10 选 5 同理，多写几个 -i）
sudo ./bin/vibe-switch -i eth0 -i eth1

# VLAN：access / trunk 混合
sudo ./bin/vibe-switch -access eth0:10 -access eth1:10 -trunk eth2:10,20

# 可选：-ageing <秒>（默认 300，0=不老化）、-ctl-sock <路径>（默认 /run/vibe-switch.sock）
```

## 管理接口 ctl

二进制运行时在 Unix socket 上提供 net/rpc 服务；`ctl` 子命令连上去查只读状态。

```bash
sudo ./bin/vibe-switch ctl show fdb       # MAC 地址表 (VLAN/MAC/端口/年龄)
sudo ./bin/vibe-switch ctl show ports     # 端口角色与 VLAN 配置
sudo ./bin/vibe-switch ctl show stats     # 每端口收发/泛洪/丢弃计数
sudo ./bin/vibe-switch ctl show config    # 端口数 / VLAN-aware / 老化时间
sudo ./bin/vibe-switch ctl show all       # 以上全部
sudo ./bin/vibe-switch ctl                # 交互式 shell：输 show fdb 等
```

## 验证

```bash
make build                                   # 编译全部（免 root）
SWITCH=goswitch sudo -E go test ./test -v    # 全套行为用例 1-11 + 性能
sudo scripts/demo.sh                         # 或 make demo：veth 拓扑上跑二进制并 ping + 查 FDB
```

本机实测：用例 1-11 全部 PASS（与 Linux bridge 行为一致）；`-race` 干净；吞吐量级与 bridge 可比
（64B ~150k pps，受控 100k pps 0% 丢包）。
