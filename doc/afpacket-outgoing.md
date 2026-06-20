# AF_PACKET 与"出向帧"——回灌、过滤与 PACKET_IGNORE_OUTGOING

本文记录围绕 [internal/goswitch/afpacket.go](../internal/goswitch/afpacket.go) 里那段 `InboundBPF`(丢弃
`PACKET_OUTGOING`)的一轮讨论与实测结论。核心问题:

> 一个 AF_PACKET socket 从某网卡发出去的帧,会不会被回灌进它**自己**的 RX ring?这个过滤器到底在防什么?有没有更对口的内核选项?

结论先行:

- **同一个 socket 发出去的帧不会回到它自己的 RX**——内核有专门代码阻止(`skb_loop_sk`)。
- 因此过滤器**不是**用来"防自己听见自己"的;它真正挡的是**同一网卡上其他来源**的出向帧。
- 对本项目"一网卡一 socket"的交换端口,这个过滤器是**防御性/基本冗余**的;对 harness 的 probe **是必需的**——
  且实测仅由一个用例 `TestL2NoReflection`(同 host 既注入又抓包)支撑,去掉即假阳性失败(见 §3)。
- 想要"RX 不收出向帧"更对口的机制是 `PACKET_IGNORE_OUTGOING`,但 gopacket v1.6.1 未暴露,需绕路。

> ⚠️ 这里更正了讨论早期的一个错误说法:"同一个 handle 会读回自己发的帧、去掉过滤器就会广播风暴"。这是**错的**,见下文实测。

---

## 1. 内核机制:出向帧的 tap 与"不回灌发送者"

用 AF_PACKET(`SOCK_RAW` + 绑定 `ETH_P_ALL`,gopacket 即如此)发包,`WritePacketData` 走的是 `unix.Write`
(`packet_sendmsg → packet_snd → dev_queue_xmit`)。发送路径最终都会经过 `dev_hard_start_xmit → xmit_one`,
在那里把出向帧**复制一份分发给挂在该网卡上的监听 socket**(即注册到 `ptype_all` 的 packet socket,每个绑了
`ETH_P_ALL` 的 AF_PACKET socket 算一个;内核函数名里的 `nit` 即 "network interface tap"):

```c
if (dev_nit_active(dev))          // 该网卡上有 ETH_P_ALL 监听 socket?
    dev_queue_xmit_nit(skb, dev); // 把副本(pkt_type=PACKET_OUTGOING)分发给这些监听 socket
```

`dev_queue_xmit_nit()` 遍历这些监听 socket 时有两道关卡:

```c
list_for_each_entry_rcu(ptype, ptype_list, list) {
    if (READ_ONCE(ptype->ignore_outgoing))   // 关卡一:PACKET_IGNORE_OUTGOING
        continue;
    /* Never send packets back to the socket they originated from - MvS */
    if (skb_loop_sk(ptype, skb))             // 关卡二:不回灌发送者
        continue;
    ...
}
```

要点:`skb_loop_sk` 只豁免**发送帧的那一个 socket**;同一网卡上的**其他** ETH_P_ALL socket 不豁免,照样
收到这份 `PACKET_OUTGOING` 副本。

---

## 2. 实测验证(kernel 6.8 + gopacket v1.6.1)

用项目同款 `gopacket/afpacket` 选项,在一对 veth 上对照测试(发包在 `vth0`,在 `vth0` 发出的帧物理上到达对端
`vth1`,故 `vth0` 自身能"读回"只可能来自 `PACKET_OUTGOING` 副本):

| 场景 | 结果 |
|---|---|
| **同一个 handle** 发完在自己身上读(无 BPF) | 读回自己的帧 = **0** |
| 同 handle + BPF | 0(无差别) |
| handle A 发、**另一个** handle B 在**同一网卡**嗅(无 BPF) | B 看到 = **1** |
| A 在 vth0 发、B 在对端 vth1 嗅 | 看到 = 1(证明帧确实发出去了) |
| B 设 `PACKET_IGNORE_OUTGOING` 后,A 在同网卡发 | B 看到 = **0** |

第一行直接证明:同一 socket 不会回灌自己的 TX(即使不挂任何过滤器)。第三行说明:过滤器真正拦的是**别的
socket** 的出向帧。

---

## 3. 那这个 BPF 过滤器到底有什么用?

`skb_loop_sk` 已经挡住"自己发的帧",所以过滤 `PACKET_OUTGOING` 真正挡掉的是:

1. **同一网卡上另一个 packet socket** 发出去的帧。这正是 harness probe 的场景。注意它的注入与捕获是
   **两个不同的 socket**:`Inject` 临时开一个 handle 写帧即关([probe.go:152](../internal/harness/probe.go#L152)),
   `Capture` 另开一个 handle 收帧([probe.go:204](../internal/harness/probe.go#L204))。当某用例在同一台 host 上
   既注入又抓包(用于断言"交换机不会把帧回灌给发送端")时,注入帧会以 `PACKET_OUTGOING` 出现在**捕获
   socket** 上(`skb_loop_sk` 只豁免注入 socket,不豁免捕获 socket),不滤掉就会被误判成"收到了"。
   `inboundFilter` 正是干这个。

   **实测确认它在当前测试套件里确实必需,但只被一个用例触发**:[TestL2NoReflection](../test/l2_test.go#L77)
   在同一台 host 1 上 `Capture(1,2)` + `Inject(1, broadcast)`,断言 `Count(1)==0`(帧不应从入端口回灌)。

   | | 结果 |
   |---|---|
   | 有 `inboundFilter`(现状) | `TestL2NoReflection` **PASS** |
   | 去掉 `inboundFilter` | **FAIL**:`frame must not come back out the ingress port, got 1` |
   | 其余 L2 / VLAN 用例(去掉后) | 全部 **PASS**,不受影响 |

   其余用例都在**非注入 host** 上抓包,看不到注入 socket 的 `PACKET_OUTGOING`,故与该过滤器无关。
   > 注:[probe.go:52](../internal/harness/probe.go#L52) 注释里"a handle that both injects and captures"措辞不准
   > ——注入和捕获其实是两个 handle;**单个** handle 自注入自捕获在本内核上并不会看到自己(实测)。真正需要
   > 过滤的是"同接口上的第二个 socket"(这里就是抓包 socket 看见注入 socket 的出向帧)。
2. **宿主机内核协议栈**在该网卡上发出的帧(ARP/ND 等)。这些帧 `skb->sk` 不是交换端口那个 socket,
   `skb_loop_sk` 不豁免;不加过滤的话,**交换机会把宿主机自己发出去的流量当成入向帧吃进来、学 MAC、再泛洪**。
   (该条由机制推得,未单测;逻辑与上同。)若交换端口用的是无 IP、协议栈不活跃的专用网卡,这部分≈0。

**对交换端口(一网卡一 socket)**:每个端口只在自己那块网卡上发,内核已阻止自环;别的端口 socket 在别的
网卡上,也看不到。因此 [afpacket.go](../internal/goswitch/afpacket.go) 里的这个过滤器对交换端口是**防御性的**
——去掉它**不会**自环风暴,但可能让交换机吞入宿主机协议栈的出向流量。它真正必需,是在"同接口上有第二个
packet socket"(probe)或"该网卡上宿主机协议栈活跃"时。

---

## 4. 旁注:为什么只有 RxOpts 没有 TxOpts

[afpacket.go](../internal/goswitch/afpacket.go) 的 `RxOpts` 配置的全是**接收侧(RX ring)**参数(帧大小、块大小、
块数、轮询超时、`OptAddVLANHeader` 还原 802.1Q 标签)。发送侧 `WritePacketData` 本质是一次 `unix.Write(fd, pkt)`,
**不建 TX ring、无可调项**,所以没有也不需要 `TxOpts`。收发用的是同一个 handle / 同一个 fd。

---

## 5. PACKET_IGNORE_OUTGOING:更对口,但 gopacket 没暴露

如果目标就是"RX 时根本不收发送方向的包",内核 4.20+ 专门提供了 socket 选项 `PACKET_IGNORE_OUTGOING`
(`SOL_PACKET`,值 `0x17`)。它在 `dev_queue_xmit_nit` 里对该 socket 直接 `continue`,**连 skb_clone 都可能省掉**
(当该网卡上所有监听 socket 都置了此标志时,不产生克隆);相比"BPF 丢弃 pkt_type==4"(仍 clone 一份、跑一遍过滤器
再丢),更省、语义更直白。

现状(gopacket v1.6.1):

- **无对应 option**:`Opt*` 列表到 `OptVNetHdrSize` 为止,没有 ignore-outgoing 开关。
- **fd 未导出、无 `FD()` 访问器**:导出的只有 `SetBPF` / `SetEBPF` / `SetFanout` / `SetPromiscuous` / `Stats` 等。
- **但**常量在 `x/sys/unix` 里有(`unix.PACKET_IGNORE_OUTGOING = 0x17`),本机内核 6.8 也支持。

可行的几条路:

1. **反射取 fd 再 setsockopt**(已实测可用,见第 2 节末行):
   ```go
   fd := int(reflect.ValueOf(tp).Elem().FieldByName("fd").Int()) // 读未导出字段是允许的
   unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_IGNORE_OUTGOING, 1)
   ```
   今天能用,但**脆**:依赖未导出字段名 `fd`,升级 gopacket 可能静默失效;`FieldByName` 找不到会 panic,需兜底。
   **不建议上生产。**
2. **给 gopacket 加 option / 方法**(干净的长期做法):fork 加 `OptIgnoreOutgoing` 或
   `func (h *TPacket) SetIgnoreOutgoing(bool)`,内部一行 `SetsockoptInt`;可顺手提上游 PR。
3. **维持现有 `SetBPF`**:可移植、不碰反射。"用 BPF 丢 `PACKET_OUTGOING`"已是项目里的既定写法——
   `InboundBPF`([afpacket.go](../internal/goswitch/afpacket.go))与 `inboundFilter`([probe.go](../internal/harness/probe.go))
   指令**完全相同**(重复定义,可考虑合并),`perfBPF`([perf.go](../internal/harness/perf.go))则以同样的
   drop-OUTGOING 开头、再附加"只留 TestEtherType"的判断。保持 `SetBPF` 与既有风格一致。

---

## 6. 实践建议

- 本项目当前用 `SetBPF` 丢 `PACKET_OUTGOING`,**就效果而言与 `PACKET_IGNORE_OUTGOING` 等价**(都让该 socket
  收不到出向帧),差别只是后者更省一次 clone。在交换机量级下开销可忽略。
- 若只是想要"RX 不收出向帧",**保持 `SetBPF` 即可**,不必为此引入反射 hack。
- 若确实偏好 `PACKET_IGNORE_OUTGOING` 的清晰/省 clone,走方案 2(加 gopacket option),**别用反射方案上生产**。
- 记住根因:交换端口的"自环"本就被内核 `skb_loop_sk` 解决了;过滤器/选项处理的是"**其他来源**的出向帧",
  这才是该不该保留它的判断依据。

---

## 附:实测复现要点

```bash
# 造一对 veth
ip link add vth0 type veth peer name vth1
ip link set vth0 up; ip link set vth1 up

# 用 gopacket/afpacket 开 handle(同项目 RxOpts),分别测:
#   - 同一 handle 发+读     → 读回 0(skb_loop_sk 生效)
#   - 两 handle 同网卡       → 第二个读到 1(收到 PACKET_OUTGOING 副本)
#   - 第二个 handle 设       → unix.SetsockoptInt(fd, SOL_PACKET, PACKET_IGNORE_OUTGOING, 1)
#     之后再测              → 读到 0
```

(测试用的临时 Go 程序未入库;需要时按上表自行复现。)
