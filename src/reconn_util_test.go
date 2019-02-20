package gateway

func TestConnControl_NotifyClosed(t *testing.T) {
	control := NewConnControl()

	wg := new(sync.WaitGroup)
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func() {
			assert.NoError(t, control.WaitClosed())
			wg.Done()
		}()
	}

	control.NotifyClosed()

	success := make(chan struct{})
	go func() {
		wg.Wait()
		success <- struct{}{}
	}()

	select {
	case <-success:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Time out")
	}
}

func TestConnControl_Close(t *testing.T) {
	control := NewConnControl()

	success := make(chan struct{})
	go func() {
		if err := control.WaitClosed(); err != nil {
			if err := control.WaitClosed(); err != nil {
				success <- struct{}{}
				return
			}
		}
		t.Error("Should not have received a signal")
	}()

	control.Close()

	select {
	case <-success:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Time out")
	}
}

func TestConnControl_IfConnAlreadyClosedNotBlocking(t *testing.T) {
	control := NewConnControl()
	control.NotifyClosed()

	success := make(chan struct{})
	go func() {
		assert.NoError(t, control.WaitClosed())
		success <- struct{}{}
	}()

	select {
	case <-success:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Time out")
	}
}

func TestConnControl_NotifyReopened(t *testing.T) {
	control := NewConnControl()

	wg := new(sync.WaitGroup)
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func() {
			assert.NoError(t, control.WaitReopened())
			wg.Done()
		}()
	}

	control.NotifyReopened()

	success := make(chan struct{})
	go func() {
		wg.Wait()
		success <- struct{}{}
	}()

	select {
	case <-success:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Time out")
	}
}

func TestConnControl_NotifyFailReopened(t *testing.T) {
	control := NewConnControl()
	control.status = CLOSE

	wg := new(sync.WaitGroup)
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func() {
			assert.Error(t, control.WaitReopened())
			wg.Done()
		}()
	}

	control.NotifyFailReopened()

	success := make(chan struct{})
	go func() {
		wg.Wait()
		success <- struct{}{}
	}()

	select {
	case <-success:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Time out")
	}
}
