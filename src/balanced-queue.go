package utils

type BalancedQueue interface {
	AddTask(id string, fn ObjectProcessor, data interface{})
}

type queueTask struct {
	fn   ObjectProcessor
	data interface{}
}

type ObjectProcessor func(interface{})

type balancedQueue struct {
	name string
	ctx  context.Context

	busyProc chan struct{}

	NotifyBorder int
	NotifyStep   int

	mu         sync.Mutex
	processors map[string]chan queueTask

	chPool sync.Pool

	opts BalancedQueueOpts
}

func (bq *balancedQueue) AddTask(id string, fn ObjectProcessor, data interface{}) {
	bq.mu.Lock()
	ch, ok := bq.processors[id]
	if !ok {
		ch = bq.chPool.Get().(chan queueTask)

		bq.processors[id] = ch
		if bq.isExceedWarnLimit(len(bq.processors), bq.opts.MaxProcessors, bq.opts.NotifyCoeffMaxProcs, bq.opts.NotifyProcStep) {
			log.Warnf("Too many %s processors: %d", bq.name, len(bq.processors))
		}
	}

	ch <- queueTask{fn, data}
	if bq.isExceedWarnLimit(len(ch), bq.opts.MaxQueueSize, bq.opts.NotifyCoeffMaxTask, bq.opts.NotifyTaskStep) {
		log.Warnf("Too many %s task in queue: %d with id: %s", bq.name, len(ch), id)
	}
	// Нельзя использовать defer, потому что тогда строчка bq.busyProc <- struct{}{} попадет в лок,
	// если на ней залочится поток, то процесс никогда не сможет завершиться в методе balancedQueue.closeProcessor
	bq.mu.Unlock()

	if !ok {
		bq.busyProc <- struct{}{}
		go bq.processor(id, ch)
	}
}

func (bq *balancedQueue) isExceedWarnLimit(size, limit int, borderCoeff float32, step int) bool {
	border := int(float32(limit) * borderCoeff)
	return (size == border) || (size > border && (size-border)%step == 0)
}

func (bq *balancedQueue) closeProcessor(id string) error {
	bq.mu.Lock()
	defer bq.mu.Unlock()
	if ch, ok := bq.processors[id]; ok {
		if len(ch) != 0 {
			return errQueueIsNotEmpty
		}

		delete(bq.processors, id)
		select {
		case <-bq.busyProc:
		default:
		}
		bq.chPool.Put(ch)
	}
	return nil
}

var errQueueIsNotEmpty = errors.New("queue is not empty")

func (bq *balancedQueue) processor(name string, ch <-chan queueTask) {
	defer trace.StartRegion(bq.ctx, name).End()
	for {
		select {
		case proc := <-ch:
			proc.fn(proc.data)
		default:
			if err := bq.closeProcessor(name); err != nil {
				switch err {
				case errQueueIsNotEmpty:
					continue
				default:
					log.WithField("processor", bq.name).WithField("name", name).Errorln(err)
				}
			}
			return
		}
	}
}

type BalancedQueueOpts struct {
	NotifyCoeffMaxProcs float32 // Log if processors count will exceed percentage of max processors [0..1]
	NotifyCoeffMaxTask  float32 // Log if task count in queue will exceed percentage of max task in queue [0..1]
	NotifyProcStep      int     // When repeat logging
	NotifyTaskStep      int     // When repeat logging
	MaxProcessors       int     // How many processors will be created
	MaxQueueSize        int     // How big queue for tasks will be
}

func (opts BalancedQueueOpts) process() BalancedQueueOpts {
	if opts.MaxProcessors <= 0 {
		opts.MaxProcessors = 5000
	}

	if opts.MaxQueueSize <= 0 {
		opts.MaxQueueSize = 50
	}

	if opts.NotifyTaskStep <= 0 {
		opts.NotifyTaskStep = 1
	}

	if opts.NotifyProcStep <= 0 {
		opts.NotifyProcStep = 1
	}

	if opts.NotifyCoeffMaxProcs < 0 {
		opts.NotifyCoeffMaxProcs = 0
	} else if opts.NotifyCoeffMaxProcs > 1 {
		opts.NotifyCoeffMaxProcs = 1
	}

	if opts.NotifyCoeffMaxTask < 0 {
		opts.NotifyCoeffMaxTask = 0
	} else if opts.NotifyCoeffMaxTask > 1 {
		opts.NotifyCoeffMaxTask = 1
	}

	return opts
}

func NewBalancedQueue(name string, bqo BalancedQueueOpts) *balancedQueue {
	ctx, _ := trace.NewTask(context.Background(), name)
	bqo = bqo.process()
	bq := &balancedQueue{
		name:       name,
		ctx:        ctx,
		processors: make(map[string]chan queueTask),
		busyProc:   make(chan struct{}, bqo.MaxProcessors),
		chPool: sync.Pool{
			New: func() interface{} {
				return make(chan queueTask, bqo.MaxQueueSize)
			},
		},
		opts: bqo,
	}
	return bq
}
