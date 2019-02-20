package utils

var _ = Describe("BalancedQueue", func() {

	Context("Single Queue", func() {
		type testData struct {
			id        string
			processed bool
		}

		var (
			bq          BalancedQueue
			processor   ObjectProcessor
			waitSuccess func(timeout time.Duration) bool
		)

		BeforeEach(func() {
			bq = NewBalancedQueue("test", BalancedQueueOpts{
				MaxProcessors: 1,
				MaxQueueSize:  10,
			})

			successCh := make(chan struct{}, 10)

			processor = func(obj interface{}) {
				task, ok := obj.(*testData)
				if !ok {
					Fail("wrong type")
				}
				task.processed = true
				successCh <- struct{}{}
			}

			waitSuccess = func(timeout time.Duration) bool {
				select {
				case <-successCh:
					return true
				case <-time.After(timeout):
					return false
				}
			}
		})

		It("first task has been processed", func() {
			task := new(testData)
			bq.AddTask("single", processor, task)
			Expect(waitSuccess(200 * time.Millisecond)).Should(BeTrue())
			Expect(task.processed).To(BeTrue())
		})

		It("several task with different id has been processed", func() {
			for _, task := range []*testData{
				{id: "1"},
				{id: "2"},
				{id: "3"},
			} {
				bq.AddTask(task.id, processor, task)
				Expect(waitSuccess(200 * time.Millisecond)).Should(BeTrue())
				Expect(task.processed).To(BeTrue())
			}
		})

		It("several task with same id has been processed", func() {
			for _, task := range []*testData{
				{id: "1"},
				{id: "1"},
				{id: "1"},
			} {
				bq.AddTask(task.id, processor, task)
				Expect(waitSuccess(200 * time.Millisecond)).Should(BeTrue())
				Expect(task.processed).To(BeTrue())
			}
		})
	})

	Context("Couple Queues", func() {
		type testData struct {
			id        string
			order     int
			processed bool
		}

		const tasksCount = 300

		var (
			bq        BalancedQueue
			processor ObjectProcessor
			waitTask  func(timeout time.Duration) *testData
			tasks     []*testData
		)

		BeforeEach(func() {
			bq = NewBalancedQueue("test", BalancedQueueOpts{
				MaxProcessors: 20,
				MaxQueueSize:  tasksCount,
			})

			successCh := make(chan *testData, tasksCount*3)

			processor = func(obj interface{}) {
				task, ok := obj.(*testData)
				if !ok {
					Fail("wrong type")
				}
				task.processed = true
				successCh <- task
			}

			waitTask = func(timeout time.Duration) *testData {
				select {
				case task := <-successCh:
					return task
				case <-time.After(timeout):
					return nil
				}
			}

			tasks := make([]*testData, 0, tasksCount*3)
			for i := 0; i < tasksCount; i++ {
				tasks = append(tasks, &testData{id: "1", order: i})
				tasks = append(tasks, &testData{id: "2", order: i})
				tasks = append(tasks, &testData{id: "3", order: i})
			}
		})

		It("task with same id must be processed in order", func() {
			var processedTasks []*testData
			for _, task := range tasks {
				bq.AddTask(task.id, processor, task)
				processedTask := waitTask(200 * time.Millisecond)
				Expect(processedTask).ShouldNot(BeNil())
				Expect(processedTask.processed).To(BeTrue())
				processedTasks = append(processedTasks, processedTask)
			}

			completedTask := make(map[string]int, 3)
			for _, task := range processedTasks {
				if lastOrder, exist := completedTask[task.id]; exist {
					Expect(lastOrder).To(Equal(task.order - 1))
				}
				completedTask[task.id] = task.order
			}
		})
	})
})
