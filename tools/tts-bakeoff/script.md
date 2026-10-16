Let's talk about how TCP decides how fast to send. This is called congestion control, and it is one of the quiet reasons the internet works at all.

Here's the problem. Your laptop wants to send a large file to a server on the other side of the world. Between the two of you sit dozens of routers, and every one of them has a small queue of packets waiting to go out. Your laptop can't see any of those queues. It doesn't know how fast the slowest link is, and it doesn't know how many other people are sharing it. And yet, somehow, it has to pick a sending rate that's fast enough to be useful, but not so fast that it floods the network.

So TCP does something clever. It doesn't try to measure the network directly. Instead, it probes. It sends a little, watches what comes back, and adjusts.

There are three parts to this. First, the congestion window. Second, slow start. And third, what happens when something goes wrong.

Let's start with the congestion window, which you'll often see written as C-WND. The congestion window is simply a limit on how much data the sender is allowed to have in flight at once. In flight means sent, but not yet acknowledged. If the window is ten packets, the sender can push out ten packets, and then it has to wait. Every time an acknowledgement comes back, often called an ACK, a slot opens up, and another packet can go.

Notice what that means. The sending rate isn't set by a timer. It's set by the rhythm of acknowledgements. Roughly, the rate is the window size divided by the round-trip time, the R-T-T. A window of ten packets, with a round trip of one hundred milliseconds, gives you about a hundred packets per second. Double the window, and you double the rate. This idea is called self-clocking, and it's elegant, because the network itself paces the sender.

Now, where does the window start? That's the second part: slow start. The name is a little misleading, because slow start is actually quite aggressive. A new connection begins with a small window, typically ten packets on modern systems. Then, for every acknowledgement that comes back, the window grows by one packet. Think about what that does over a round trip. You send ten, you get ten ACKs, and the window becomes twenty. Next round trip, twenty becomes forty. Then eighty. The window doubles every round trip. That's exponential growth, and it means a connection can find a lot of bandwidth in just a handful of round trips.

But exponential growth can't go on forever. At some point, the sender is pushing more than the path can carry. Packets start piling up in a router's queue, the queue fills, and the router has no choice but to drop packets on the floor.

Which brings us to the third part: what happens when something goes wrong.

For most of TCP's history, the signal for congestion has been packet loss. When a packet goes missing, the sender assumes it pushed too hard. There are two ways it can notice. The fast way is duplicate acknowledgements. The receiver keeps acknowledging the last packet it got in order, so if packet five goes missing, but six, seven and eight arrive, the sender sees the same acknowledgement for packet four, again and again. Three duplicate ACKs in a row is treated as a strong hint that one packet was lost, while the rest of the pipe is still flowing. The sender retransmits that one packet right away. This is called fast retransmit.

The slow way is a timeout. If nothing comes back at all for long enough, the retransmission timer fires. A timeout is a much worse sign. It suggests the path is badly congested, or something has broken entirely. So TCP reacts harshly: it drops the window all the way back to its starting size and begins slow start again.

Here's a question worth pausing on. Why should a timeout be punished so much harder than three duplicate ACKs? Think about it for a second. The answer is that duplicate ACKs prove that packets are still getting through. The network is congested, but it's working. A timeout proves nothing is getting through. Backing off hard is the polite thing to do.

Now let's put these pieces together into the classic scheme, which goes by the name AIMD. That stands for additive increase, multiplicative decrease.

After slow start ends, TCP switches into a gentler mode called congestion avoidance. In congestion avoidance, instead of doubling every round trip, the window grows by about one packet per round trip. That's the additive increase. Slow and steady, feeling for the edge. And when a loss is detected through duplicate ACKs, the window is cut in half. That's the multiplicative decrease.

If you graph the window over time, you get a sawtooth. It climbs in a straight line, drops by half, climbs again, drops again. That sawtooth is the sound of TCP constantly testing the limit of the path, and politely backing away when it hits it.

Why this particular shape? Because it's fair. Imagine two connections sharing one bottleneck link. If one has a bigger window than the other, then when they both see a loss and both cut in half, the bigger one gives up more in absolute terms. Meanwhile, they both grow by the same one packet per round trip. Over many cycles, the two windows drift towards each other. Nobody coordinated anything, and yet the link ends up shared roughly equally. That's the real genius of AIMD. Fairness emerges from a simple local rule.

So far we've described the classic algorithm, often called Reno. But the internet has changed a lot since Reno was designed. Links got much faster and much longer. Think of a ten-gigabit link across an ocean. On a path like that, the window you need is enormous, and growing it by one packet per round trip after a loss could take many minutes. Reno simply can't fill the pipe.

That's why most Linux servers today use an algorithm called CUBIC. CUBIC still cuts the window when it sees a loss, but it grows the window using a cubic function of time since the last loss, rather than one packet per round trip. In practice, this means it grows quickly when it's far below the point where it last saw trouble, slows down and carefully probes as it gets close to that point, and then speeds up again if it gets past it without a loss. The growth depends on time, not on round trips, so connections with long round-trip times aren't left behind.

And then there's a completely different idea, from Google, called BBR. That stands for bottleneck bandwidth and round-trip propagation time. BBR's insight is that packet loss is a late and noisy signal. By the time a router drops packets, its queue has been full for a while, and every packet has been sitting in it, adding delay. So BBR doesn't wait for loss. Instead, it continuously estimates two things: the bottleneck bandwidth, which is the fastest delivery rate it has recently seen, and the minimum round-trip time, which is the delay when the queues are empty. Multiply those together and you get the amount of data the path can hold without building a queue. BBR tries to keep exactly that much in flight. It's less like a sawtooth, and more like cruise control.

There's one more tool worth knowing about, called E-C-N, explicit congestion notification. With ECN, instead of dropping a packet when its queue starts filling, a router can mark the packet with a flag that says, I'm getting congested. The receiver echoes that mark back to the sender, and the sender slows down, without anything being lost. It's the network tapping you on the shoulder, rather than slamming a door.

Let's recap. TCP can't see the network, so it probes. The congestion window caps how much is in flight, and the stream of acknowledgements paces the sender. Slow start grows the window exponentially to find capacity quickly. Loss, detected through duplicate ACKs or a timeout, tells the sender it went too far, and it backs off: by half for duplicate ACKs, and all the way back for a timeout. Additive increase and multiplicative decrease give you the sawtooth, and that sawtooth gives you fairness. CUBIC adapts this to fast, long paths, BBR replaces loss with a model of the path, and ECN lets routers warn senders before anything is dropped.

Here's something to think about before the next part. If BBR deliberately avoids building queues, and CUBIC keeps pushing until a queue overflows, what do you think happens when a BBR connection and a CUBIC connection share the same bottleneck? We'll pick that up next.
