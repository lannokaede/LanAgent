package interview

// Reference answers are server-owned study notes, not generated during review.
// The live reviewer compares a candidate answer with these and retrieved docs.
var referenceAnswers = map[string]string{
	"channel-sync":   "发送操作 synchronized before 对应接收操作完成。对无缓冲 channel，接收操作还 synchronized before 对应发送操作完成。结合 goroutine 内的程序顺序，这些规则可建立跨 goroutine 的 happens-before 链，使链上较早的写入对较晚的读取可见。不要将规则改写成‘发送完成先于接收开始’；同步本身也不能消除程序中其他未同步访问的数据竞争。",
	"context-cancel": "父 context 取消会向派生 context 传播，goroutine 应 select 监听 ctx.Done()，在退出路径释放资源。取消是协作式通知，不会强行停止任意 goroutine，也不证明工作已经退出；需要 WaitGroup、结果 channel 等等待完成。阻塞 I/O 也应使用支持 context 或有期限的操作。创建可取消 context 后应调用 cancel 释放相关资源。",
	"pipeline-leak":  "下游不再接收时，上游可能永久阻塞在发送上。用共享取消信号或 context，使发送和接收路径都能选择退出；明确由谁关闭 channel，并等待所有 worker 完成。只增加缓冲区通常不能解决任意数量未消费结果，错误退出路径也必须传播取消。",
	"map-concurrent": "普通 map 的并发读写需要同步，否则存在数据竞争；没有任何并发写入时可以并发读取。可用 Mutex/RWMutex 保护复合操作，或由单个 goroutine 持有数据并通过 channel 交互。选择取决于访问模式、所有权和实测瓶颈，不能仅因使用 channel 就保证所有其他共享数据安全。",
	"slice-append":   "slice 描述符引用底层数组，包含长度和容量。共享数组的切片在重叠范围内修改元素会相互可见。append 在容量够时可能复用数组，容量不够时分配新数组，因此不能假设 append 总会或总不会影响其他切片。需要独立数据时显式复制；并发访问仍需同步。",
	"interface-nil":  "interface 值包含动态类型和动态值；只有两者都未设置时才是 nil。将 (*T)(nil) 赋给 interface 后，动态类型为 *T，动态值是 nil 指针，因此该 interface 不等于 nil。错误接口返回值中也会出现这个陷阱，应在确实无错误时返回字面量 nil。",
	"gc-cost":        "GC 的成本与存活堆、分配速率、扫描工作和回收频率有关。较大的 GOGC 通常允许更大的堆目标，以更多内存换取更低的 GC 频率与部分 CPU 开销；更小的值通常反向取舍。实际还受内存限制和负载影响，应结合 profile 和运行指标判断，不能保证某个参数对所有负载都更快。",
	"heap-escape":    "编译器按值的生命周期和引用关系决定存储位置；引用逃出当前调用或大小等信息无法在编译期确定时可能需要堆分配。返回局部变量的指针在 Go 中是合法的，由实现保证生命周期。new、取地址或指针类型本身都不等于必然堆分配，应检查编译器逃逸分析和分配 profile。",
	"sql-pool":       "sql.DB 是可并发使用的连接池句柄，不是一条固定连接。SetMaxOpenConns 限制打开的连接数，达到上限后请求等待可用连接；设置过小可能增加排队，持有连接又等待新连接的流程还可能死锁。结合 DB.Stats、数据库容量、超时、Rows.Close 和事务生命周期调节连接池。",
	"sql-tx":         "事务应通过 BeginTx 创建，并在同一 sql.Tx 上执行相关操作，成功 Commit，失败 Rollback。混用 sql.DB 会使部分操作在事务外、甚至不同连接上执行，破坏一致性，且在连接额度不足时可能互相等待。事务尽量短，并传递 context 处理取消。",
	"isolation":      "在 PostgreSQL 中，Read Committed 通常为每条语句取得新的快照，同一事务两次查询可能看到不同已提交数据；Repeatable Read 基于事务快照，仍可能发生需要重试的并发失败。隔离级别定义可见性与异常限制，MVCC 不代表不加锁；写入、显式锁和约束依然可能冲突。其他数据库同名级别的细节要分别确认。",
	"locks":          "事务按不同顺序持有并等待彼此需要的锁时可能死锁。让所有事务按一致顺序加锁、缩短事务并避免持锁期间的外部等待可降低风险。数据库检测死锁后会中止一个事务，应用应在可重试条件下重试整个事务，并处理幂等与退避；不能只重试事务中的单条语句。",
	"redis-aof-rdb":  "RDB 在某个时点生成快照，适合备份且通常便于较快恢复，但最近快照后的修改可能丢失。AOF 记录写操作，持久性取决于 fsync 策略，并涉及重写与恢复成本；不能承诺绝不丢数据。应结合可接受丢失窗口、恢复时间、磁盘与 CPU 成本选择或组合，并实际验证备份恢复。",
	"redis-type":     "排行榜通常使用 Sorted Set：成员唯一，每个成员关联分数，可按分数排序、查询排名和范围。普通 Set 适合唯一成员与集合操作，但不提供同样的分数排序语义。需要进一步明确并列分数、更新方式、规模和业务分页要求。",
	"race":           "race detector 在实际执行的插桩代码路径中检测数据竞争。一次通过仅说明本次覆盖与调度下未观察到竞争，不能证明未执行路径安全，也不能证明没有死锁、逻辑竞态或跨进程一致性问题。应配合有代表性的并发测试、真实负载和代码审查。",
	"pprof":          "先明确 CPU、内存、阻塞或 goroutine 异常，并在可控时间窗采集对应 profile。CPU profile 用于定位耗时热点；heap/alloc profile 区分存活内存与分配压力，goroutine、mutex、block profile 帮助分析等待。结合指标和相同条件下的对比验证假设，避免仅凭一次截图下结论。",
}
