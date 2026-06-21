> [中文版 README](./README_zh.md)

## Preface

I already had a basic understanding of how a switch works at the software level: [Introduction to Switches - da1234cao](https://www.da1234cao.space/introduction-to-switches/). Now that we have AI, I wanted to let the AI take a crack at writing one.

Work experience shapes the choices you make. I wanted to build a switch that runs in Linux user space. So when sending and receiving Layer 2 frames, I need to bypass the kernel network stack.

There are several ways to make packets bypass the kernel network stack.
- DPDK can bypass the kernel stack. I've written a bit with it before ([da1234cao/dpdk-exercise](https://github.com/da1234cao/dpdk-exercise)). But DPDK is too heavy — it not only dictates how packets are sent and received, it even forces the code into a node-graph style of implementation. I didn't want to use it.
- AF_XDP can let packets matching certain criteria bypass the kernel stack. I've written a bit with it before ([da1234cao/ebpf-arp](https://github.com/da1234cao/ebpf-arp)). But XSK (the AF_XDP socket) is genuinely hard to use. Even with AI assistance, that interface felt a little bizarre and awkward.
- af_packet doesn't perform as well as the two above. From a usability standpoint it's still a bit involved. That's fine — I had the AI write a tutorial; reading code is easier than reading docs ([da1234cao/af\_pakcet\_tutorial](https://github.com/da1234cao/af_pakcet_tutorial)).

Alright — decision made: use af_packet as the access method to implement a user-space switch.

## Design

The first step is to write the tests. Writing tests first has a few benefits.
- The process of writing tests pins down the interface the switch exposes to users. A switch may have different internal implementations, but as a class of product it should have a similar external interface.
- Linux ships with a built-in bridge (switch). We first write a set of black-box test cases against it. Later, we use that same set of test cases to test our own switch.
- How do you write the tests? Let the AI write them: [TDD](./doc/TDD.md)

The second step is to implement the switch.
- The implemented switch has to pass the tests above.
- Decided on the approach: implement a user-space switch with af_packet.
- Don't couple the (af_packet) access method to the switch's internal implementation — leave room to swap in a different access method later.
- How do you write the implementation? Let the AI write it: [switch](./doc/switch.md)

## Running

Build

```
root@ubuntu24-1 ~/w/s/vibe-switch (main)# make build-bin
go build -o ./bin/vibe-switch ./cmd/vibe-switch
```

Pass the test cases

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

Try it out in a VMware virtual machine.

```
root@ubuntu24-1 ~/w/s/vibe-switch (main)# ./bin/vibe-switch -i ens37 -i ens38
port ens37 role=plain
port ens38 role=plain
vibe-switch up: 2 ports, ctl socket /run/vibe-switch.sock (Ctrl-C to stop)

```

Check the forwarding rate.

```
root@ubuntu24-1 ~/w/s/vibe-switch (main)# ./bin/vibe-switch ctl show rate
PORT   RX_PPS   RX_RATE        TX_PPS   TX_RATE        FWD_DROP_PS  TX_DROP_PS
ens37  11460.9  105.03 Mbit/s  12565.4  121.86 Mbit/s  0.0          0.0
ens38  12565.4  121.86 Mbit/s  11459.9  105.01 Mbit/s  0.0          0.0
```

## Closing thoughts

AI is remarkably good at writing code and fixing bugs. With AI around, from now on I think I'll only write two kinds of code.
- One kind is the code my job requires — after all, I have to make a living.
- The other is the code I'm interested in. For that kind, I'll no longer review its internal implementation; I'll just use it.
