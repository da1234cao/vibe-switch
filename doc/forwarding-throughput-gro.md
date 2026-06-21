# 转发吞吐极低排查——GRO 把帧合并到超过 MTU，出口被静默丢弃

本文记录一次实战排查:用两块真实网卡跑 `vibe-switch` 做二层转发,`iperf3` 实测吞吐只有
**225 Kbit/s**,排查到根因是网卡 **GRO(generic-receive-offload)** 把入向帧合并成超过 MTU 的
"超大帧",转发到出口网卡时被内核以 `EMSGSIZE` 拒绝,而 [emit](../internal/goswitch/engine.go#L255)
又把这个写出错误**静默吞掉**,导致丢包在统计里完全隐身。

> 环境:kernel 6.8、gopacket/afpacket v1.6.1,`./bin/vibe-switch -i ens37 -i ens38`(两个 plain 口,
> 一个广播域),两端各一台主机 10.0.0.1 / 10.0.0.2 经交换机互通。

结论先行:

- **任何基于 AF_PACKET 的软件网桥/交换机,都必须关掉入口网卡的 GRO**(及 LRO),否则内核会在帧到达
  AF_PACKET tap **之前**就把多个 TCP 段合并成 >MTU 的大帧,而二层转发无法重新分片,只能丢。
- 本次现象的"放大器"是代码 bug:[emit](../internal/goswitch/engine.go#L266-L272) 对
  `WritePacketData` 的返回值**只在成功时计数,失败时什么都不做**——既不计入 `DROPPED`,也不记日志。
  于是 17% 的帧人间蒸发,`show stats` 却显示 `DROPPED 0`,一切"正常"。
- 立刻可用的修复:`ethtool -K <iface> gro off gso off tso off lro off`(注意**重启/驱动重载后失效**,需持久化)。
- 建议的长期修复:`OpenInterface` 里自动关 offload;`emit` 把写出错误显式计数;对超过出口 MTU 的帧显式丢弃并计数。

---

## 1. 现象

```text
root@localhost ~# iperf3 -c 10.0.0.2
[ ID] Interval           Transfer     Bitrate         Retr  Cwnd
[  5]   0.00-1.02   sec  76.4 KBytes   615 Kbits/sec   16   2.83 KBytes
[  5]   1.02-2.01   sec  31.1 KBytes   258 Kbits/sec   14   2.83 KBytes
...
[  5]   0.00-10.01  sec   512 KBytes   419 Kbits/sec  146             sender
[  5]   0.00-18.64  sec   512 KBytes   225 Kbits/sec                  receiver
```

关键特征:

- **cwnd 死死卡在 2.83 KB**(约 2 个 MSS):拥塞窗口一打开就被丢包打回去,根本张不开。
- **持续重传**(10 秒 146 次):数据段反复丢失重发。

---

## 2. 排查过程

### 2.1 先看网卡 offload

cwnd 崩 + 持续重传 ⇒ 链路在丢包。软件交换机自己就是"链路",先排查它的两块网卡。

```bash
$ ethtool -k ens37 | grep -E 'generic-receive|tcp-segmentation|generic-segmentation|large-receive'
tcp-segmentation-offload: on
generic-segmentation-offload: on
generic-receive-offload: on          # ← 嫌疑最大
large-receive-offload: off [fixed]
$ ip -br link show ens37          # MTU 1500
```

`GRO on`。GRO 会在 NAPI 收包阶段把同一条流的多个段**合并**成一个大 skb,**早于** AF_PACKET 的
`ptype_all` tap(`__netif_receive_skb_core` 里的抓取点)。也就是说:交换机从 ring 里读到的,可能是
已经被内核合并、**长度超过 1500 的帧**。这正是 "tcpdump 抓到比 MTU 还大的包" 的同款效应。

### 2.2 再看交换机自己的统计——铁证

```text
$ ./bin/vibe-switch ctl show stats
PORT   RX_FRAMES  RX_BYTES  TX_FRAMES  TX_BYTES  FLOODED  DROPPED
ens37  1271       2165865   861        62438     2        0          # 入口:大块数据
ens38  861        62438     1054       1517319   0        0          # 出口
```

两处异常:

1. **帧数对不上**:ens37 收了 `1271` 帧,本该全部从 ens38 转发出去,但 ens38 只发了 `1054` 帧——
   **凭空少了 217 帧(约 17%)**,而两个口的 `DROPPED` 都是 `0`。这些帧既没被解析丢弃、也没被
   VLAN 分类丢弃,却消失了。

2. **平均帧长超 MTU**:ens37 入向平均帧长 `2165865 / 1271 ≈ 1704 字节` > 1500;而真正从 ens38
   发出去的帧平均才 `1517319 / 1054 ≈ 1440 字节`。**大的那些没出去。**

反向对照更干净:ens38 入向 `861 帧 / 62438 字节`(平均 ~72 字节,是 ACK),ens37 出向 `861 / 62438`
**完全相等**——小帧方向**零丢失**。只有大帧方向在丢。

`DROPPED=0` 说明丢点不在解析/分类逻辑里。结合"少的全是大帧",定位到**出口写帧**这一步。

### 2.3 看出口写帧的代码——找到静默吞错误

[engine.go](../internal/goswitch/engine.go#L255-L273) 的 `emit`:

```go
out.writeMu.Lock()
err := out.io.WritePacketData(pkt)
out.writeMu.Unlock()
if err == nil {                                  // ← 只有成功才计数
    atomic.AddUint64(&out.stats.txFrames, 1)
    atomic.AddUint64(&out.stats.txBytes, uint64(len(pkt)))
}
// err != nil 时:不计 DROPPED、不记日志、直接返回 —— 帧凭空消失
```

`WritePacketData` 本质是 `packet_snd → dev_queue_xmit`。当 `pkt` 长度超过出口网卡 MTU 时,内核在
`packet_snd` 里直接返回 **`-EMSGSIZE`**(`len > dev->mtu + …` 的检查)。于是:

> GRO 合出来的 >1500 字节大帧 → 交换机读入 → 试图从 MTU=1500 的 ens38 发出 → 内核 `EMSGSIZE` 拒绝
> → `emit` 静默吞掉 → 帧丢失,统计无感。

二层交换机**不能**像路由器那样对 L2 帧重新分片,所以这种超大帧本就无解——唯一正确做法是从源头
不让它变大(关 GRO)。

---

## 3. 根因

```
发送端网线上是正常的 ~1500B TCP 段
        │
        ▼  ens37 收包,GRO 合并(发生在 AF_PACKET tap 之前)
若干段 → 一个 >1500B 的超大帧
        │
        ▼  交换机从 RX ring 读到超大帧(FrameSize 4096,装得下,不报错)
        │
        ▼  转发:WritePacketData 到 ens38(MTU 1500)
内核返回 EMSGSIZE
        │
        ▼  emit() 只在 err==nil 时计数 → 错误被吞 → 帧丢失
        │
        ▼  TCP 持续丢数据段 → cwnd 崩到 2.83KB → 225 Kbit/s
```

承载绝大部分字节的数据段被成片丢弃,而反向小 ACK 全部通过。

> 旁注:`OptFrameSize(4096)`([afpacket.go:29](../internal/goswitch/afpacket.go#L29))让 RX ring **能装下**
> 4KB 内的合并帧(所以它们没在入口被截断/丢弃,而是被读进来),反而把问题推到了出口。即便调大
> FrameSize 也救不了——出口 MTU 摆在那,根上还是不该让帧变大。

---

## 4. 修复

### 4.1 立即修复:关掉 offload

```bash
for i in ens37 ens38; do
    ethtool -K "$i" gro off gso off tso off lro off
done
```

- `gro`(收向合并)是本案主因,**必关**。`lro` 同理(本机 `[fixed]` 已关)。
- `gso/tso`(发向分段)对"只转发、不本地起 TCP"的交换端口影响小,但软件网桥一并关掉是惯例,无害。

> ⚠️ **`ethtool -K` 不持久**:重启或网卡驱动重载即失效。要持久化,用 systemd unit 在网卡 up 后执行上述命令,
> 或走 netplan / NetworkManager 的链路设置。

关掉后所有帧 ≤ MTU,出口不再 `EMSGSIZE`,转发恢复,吞吐应有数量级提升。

### 4.2 长期修复(代码侧)

本次能从 `225 Kbit/s` 一路查到根因,靠的是交换机统计;但同样是统计的盲区放大了问题。建议:

1. **`emit` 不要静默吞写出错误**([engine.go:266](../internal/goswitch/engine.go#L266)):
   `WritePacketData` 失败时计入一个 `txErrors`/`txDropped` 计数器,并在 `show stats` 里展示。
   这样"收到没发出去"的帧不再隐身。

2. **`OpenInterface` 自动关 offload**([afpacket.go:80](../internal/goswitch/afpacket.go#L80)):
   开口时通过 ethtool ioctl(`ETHTOOL_SFEATURES`)/ netlink 关掉本网卡的 GRO/LRO,
   不依赖管理员记得手敲 `ethtool`。软件交换机应当自我防御。

3. **超过出口 MTU 的帧显式丢弃 + 计数**:即使将来 offload 没关干净,也应在 `emit` 里显式判断
   `len(pkt) > egress MTU` 并记一个明确的丢弃原因,而不是让内核报 `EMSGSIZE` 后被吞。

---

## 5. 旁注:这不是吞吐的全部上限

关掉 offload 解决的是**功能性丢包**(本案主因)。要继续逼近线速,还有一个结构性瓶颈:

- `emit` 里每个包一次 `WritePacketData`,即**每包一次 `sendto` 系统调用**(发送侧不建 TX ring,见
  [afpacket-outgoing.md §4](./afpacket-outgoing.md))。这会把 pps 压在远低于线速的水平。
- 提升手段:用 `PACKET_MMAP` **发送环** / `sendmmsg` 做**批量发送**;接收侧也可批量收取。
  这属于性能优化,**不是本案的 bug**——本案哪怕单包路径,关掉 GRO 后也能从 225 Kbit/s 跳上几个数量级。

---

## 6. 复现与验证要点

```bash
# 1. 复现:确认网卡 GRO 开着
ethtool -k ens37 | grep generic-receive-offload      # → on

# 2. 跑流量后看交换机统计,RX_FRAMES 明显 > 对端 TX_FRAMES、且 DROPPED=0,
#    入向平均帧长 (RX_BYTES/RX_FRAMES) > MTU,即中招
./bin/vibe-switch ctl show stats

# 3. 修复并验证
ethtool -K ens37 gro off gso off tso off lro off
ethtool -K ens38 gro off gso off tso off lro off
iperf3 -c 10.0.0.2                                    # 吞吐应大幅回升

# 旁证:抓包能直接看到 >1514B 的"超大帧"(GRO 合并),关掉 GRO 后消失
tcpdump -i ens37 -nn -e 'ip' | awk '{print $NF}'      # length 字段
```

---

## 附:经验法则

- **AF_PACKET tap 在 GRO 之后**:凡是经 AF_PACKET 抓/转发包的程序(软件网桥、交换机、镜像/分析),
  入口网卡都要关 GRO/LRO,否则会读到 >MTU 的合并帧。这是该类程序的"出厂必做项"。
- **转发路径上每一个写出错误都要可见**:`WritePacketData` 失败 = 丢包,必须计数。统计里
  "收到的帧数 ≠ 发出的帧数 + 明确丢弃数" 本身就是一条该报警的不变式。
- **诊断顺序**:cwnd 崩 + 重传 ⇒ 丢包 → 先查转发设备网卡 offload/MTU → 再查设备自身计数的
  收/发/丢是否守恒 → 守恒被破坏处即丢点。
