package gateway

type Status int

const (
	OPEN Status = iota
	FAILED
	CLOSE
)

func (s Status) String() string {
	switch s {
	case OPEN:
		return "Open"
	case FAILED:
		return "Failed"
	case CLOSE:
		return "Close"
	default:
		return "Unknown"
	}
}

// ConnControl позволяет управлять состоянием подключения.
type ConnControl struct {
	cd *sync.Cond

	status Status
	closed abool.AtomicBool
}

func NewConnControl() *ConnControl {
	return &ConnControl{
		cd: sync.NewCond(&sync.Mutex{}),
	}
}

func (c *ConnControl) WaitClosed() error {
	if c.closed.IsSet() {
		return errors.New("connection control was closed")
	}

	c.cd.L.Lock()
	defer c.cd.L.Unlock()
	for c.status != CLOSE {
		c.cd.Wait()
		if c.closed.IsSet() {
			return errors.New("connection control was closed")
		}
	}
	return nil
}

func (c *ConnControl) WaitReopened() error {
	if c.closed.IsSet() {
		return errors.New("connection control was closed")
	}

	c.cd.L.Lock()
	defer c.cd.L.Unlock()
	for c.status == CLOSE {
		c.cd.Wait()
		if c.closed.IsSet() {
			return errors.New("connection control was closed")
		}
	}
	switch c.status {
	case OPEN:
		return nil
	case FAILED:
		return errors.New("fail reopen connection")
	default:
		panic("unexpected state: " + c.status.String())
	}
}

func (c *ConnControl) NotifyClosed() {
	c.notifyAll(CLOSE)
}

func (c *ConnControl) NotifyReopened() {
	c.notifyAll(OPEN)
}

func (c *ConnControl) NotifyFailReopened() {
	c.notifyAll(FAILED)
}

func (c *ConnControl) notifyAll(status Status) {
	c.cd.L.Lock()
	c.status = status
	c.cd.Broadcast()
	c.cd.L.Unlock()
}

func (c *ConnControl) Status() Status {
	c.cd.L.Lock()
	defer c.cd.L.Unlock()
	return c.status
}

func (c *ConnControl) Close() {
	c.closed.Set()
	c.cd.Broadcast()
}
