## 前言

之前，我已经知道，交换机软件层面的基本原理：[交换机简介 - da1234cao](https://www.da1234cao.space/introduction-to-switches/)。现在又有 AI 了，我想让 AI 写个试试。

工作经历，会影响方案选择。我希望实现一个在 Linux 用户态运行的交换机。所以，我收发二层数据帧的时候，要绕过内核协议栈。

让数据包绕过内核协议栈，有多种方式。
- DPDK 可以让数据包绕过内核协议栈。我之前也写过一点([da1234cao/dpdk-exercise](https://github.com/da1234cao/dpdk-exercise))。但是，DPDK 太重了，它不仅决定了收发包的方式，甚至绑定了代码只能以 node graph 的方式实现。我不想使用它。
- AF_XDP 可以让指定特征的数据包绕过内核协议栈。我之前写过一点([da1234cao/ebpf-arp](https://github.com/da1234cao/ebpf-arp))。但是，XSK(AF_XDP socket) 实在是难用。即使有 AI 辅助，我也感觉那套接口有点奇葩，不太好用。
- af_packet 性能没有上面两者好。从使用角度来说，它还是有点复杂。没关系，让 AI 写一个教程 ，看代码比看文档要容易([da1234cao/af\_pakcet\_tutorial](https://github.com/da1234cao/af_pakcet_tutorial))

好了，确定使用 af_packet 的接入方式，实现一个用户态的交换机。

## 设计

第一步是写测试。先写测试有两个好处。
- 写测试的过程，可以确定交换机对用户暴露的接口。交换机可能有不同的内部实现，但是对于交换机这一类产品来说，它应该有相似的外部接口。
- Linux 自带 bridge 交换机。我们先为它写一组黑盒测试用例。之后，我们用这组测试用例，测试我们自己的交换机。
- 测试咋写？让 AI 写: [TDD](./doc/TDD.md)

第二步是实现交换机。
- 实现的交换机，要能通过上面的测试
- 确定路线：使用 af_packet 实现一个用户态的交换。
- 不要将 (af_packet) 接入方式，与交换机内部的实现绑定，为以后切换不同接入方式留下空间。
- 实现咋写？让 AI 写：[switch](./doc/switch.md)

## 运行

构建

```
root@ubuntu24-1 ~/w/s/vibe-switch (main)# make build-bin
go build -o ./bin/vibe-switch ./cmd/vibe-switch
```

通过测试用例

```
root@ubuntu24-1 ~/w/s/vibe-switch (main)# make test
== switch under test: SWITCH=goswitch ==
SWITCH=goswitch go test ./test -v 
=== RUN   TestL2Connectivity
--- PASS: TestL2Connectivity (1.28s)
=== RUN   TestL2KnownUnicastNotFlooded
--- PASS: TestL2KnownUnicastNotFlooded (4.09s)
=== RUN   TestL2UnknownUnicastFlooded
--- PASS: TestL2UnknownUnicastFlooded (3.10s)
=== RUN   TestL2BroadcastFlooded
--- PASS: TestL2BroadcastFlooded (3.38s)
=== RUN   TestL2NoReflection
--- PASS: TestL2NoReflection (2.55s)
=== RUN   TestL2MACAging
--- PASS: TestL2MACAging (6.34s)
=== RUN   TestPerfThroughput
    perf_test.go:36: [go-switch] throughput   64B: 157348 pps, 80.6 Mbit/s, tx=189157 rx=125879 loss=33.45%
    perf_test.go:36: [go-switch] throughput  512B: 128787 pps, 527.5 Mbit/s, tx=147295 rx=103093 loss=30.01%
    perf_test.go:36: [go-switch] throughput 1500B: 104337 pps, 1252.0 Mbit/s, tx=145416 rx=83470 loss=42.60%
--- PASS: TestPerfThroughput (5.86s)
=== RUN   TestPerfLatency
    perf_test.go:59: [go-switch] latency over 400 samples: min=180.4us p50=2150.9us p99=17930.1us max=33579.4us
--- PASS: TestPerfLatency (3.07s)
=== RUN   TestPerfStressLoss
    perf_test.go:71: [go-switch] stress @ 10000 pps target: delivered 9736 pps, loss=0.00%
    perf_test.go:71: [go-switch] stress @ 50000 pps target: delivered 49188 pps, loss=0.00%
    perf_test.go:71: [go-switch] stress @100000 pps target: delivered 98788 pps, loss=0.00%
--- PASS: TestPerfStressLoss (4.80s)
=== RUN   TestVLANSameVLANConnectivity
--- PASS: TestVLANSameVLANConnectivity (3.50s)
=== RUN   TestVLANCrossVLANIsolation
--- PASS: TestVLANCrossVLANIsolation (5.12s)
=== RUN   TestVLANAccessEgressUntagged
--- PASS: TestVLANAccessEgressUntagged (4.32s)
=== RUN   TestVLANTrunkPassthrough
--- PASS: TestVLANTrunkPassthrough (4.60s)
=== RUN   TestVLANPVIDClassification
--- PASS: TestVLANPVIDClassification (4.44s)
PASS
ok      vibe-switch/test        56.480s
```

在 vmware 虚拟机中测试下。

```
root@ubuntu24-1 ~/w/s/vibe-switch (main)# ./bin/vibe-switch -i ens37 -i ens38
port ens37 role=plain
port ens38 role=plain
vibe-switch up: 2 ports, ctl socket /run/vibe-switch.sock (Ctrl-C to stop)

```

查看转发速度。

```
root@ubuntu24-1 ~/w/s/vibe-switch (main)# ./bin/vibe-switch ctl show rate
PORT   RX_PPS   RX_RATE        TX_PPS   TX_RATE        FWD_DROP_PS  TX_DROP_PS
ens37  11460.9  105.03 Mbit/s  12565.4  121.86 Mbit/s  0.0          0.0
ens38  12565.4  121.86 Mbit/s  11459.9  105.01 Mbit/s  0.0          0.0
```

## 最后

AI 写代码，修bug的能力非常的强。有了 AI 之后，我以后应该只写两类代码。
- 一类代码是工作需要的，毕竟我需要谋生糊口。
- 另一类是我感兴趣的。这类代码，我将不再 review 它的内部实现，而是直接使用。