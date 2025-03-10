package t1k

import (
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chaitin/t1k-go/detection"

	"github.com/chaitin/t1k-go/misc"
)

const (
	DEFAULT_POOL_SIZE  = 8
	HEARTBEAT_INTERVAL = 20
)

type Server struct {
	socketFactory   func() (net.Conn, error)
	poolCh          chan *conn
	poolSize        int64
	count           int64
	closeCh         chan struct{}
	logger          *log.Logger
	SocketErrorHook func(error)

	cntlock    sync.Mutex
	configLock sync.RWMutex

	healthCheck *HealthCheckService
}

func (s *Server) UpdateSockErrorHandler(errorHandler func(error)) {
	s.configLock.Lock()
	defer s.configLock.Unlock()
	s.SocketErrorHook = errorHandler
}

// added by YF-Networks's taochunhua
func (s *Server) UpdateSockFactory(socketFactory func() (net.Conn, error)) {
	s.configLock.Lock()
	defer s.configLock.Unlock()
	s.socketFactory = socketFactory
}

// refactor by YF-Networks's yeyunxi
func (s *Server) CallSockFactory() (net.Conn, error) {
	s.configLock.RLock()
	defer s.configLock.RUnlock()
	return s.callSockFactory()
}

// added by YF-Networks's yeyunxi
func (s *Server) callSockFactory() (net.Conn, error) {
	conn, err := s.socketFactory()
	if err != nil && s.SocketErrorHook != nil {
		s.SocketErrorHook(err)
	}
	return conn, err
}

func (s *Server) newConn() error {
	sock, err := s.CallSockFactory()
	if err != nil {
		return err
	}
	s.count += 1
	s.poolCh <- makeConn(sock, s)
	return nil
}

func (s *Server) GetConn() (*conn, error) {
	var err error

	if atomic.LoadInt64(&s.count) < s.poolSize {
		s.cntlock.Lock()
		if s.count < s.poolSize {
			for i := int64(0); i < (s.poolSize - s.count); i++ {
				err = s.newConn()
				if err != nil {
					break
				}
			}
		}
		s.cntlock.Unlock()
		if err != nil {
			return nil, err
		}
	}

	c := <-s.poolCh
	if c.failing {
		err = c.tryReconnIfFailed()
		if err != nil {
			s.poolCh <- c
			return nil, err
		}
	}

	return c, nil
}

func (s *Server) PutConn(c *conn) {
	s.poolCh <- c
}

func (s *Server) broadcastHeartbeat() {
	for {
		select {
		case c := <-s.poolCh:
			if !c.failing {
				c.Heartbeat()
			}
			s.PutConn(c)
		default:
			return
		}
	}
}

func (s *Server) runHeartbeatCo() {
	interval := HEARTBEAT_INTERVAL
	intervalRaw := os.Getenv("T1K_HEARTBEAT_INTERVAL")
	if intervalRaw != "" {
		val, err := strconv.Atoi(intervalRaw)
		if err == nil {
			interval = val
		}
	}
	for {
		timer := time.NewTimer(time.Duration(interval) * time.Second)
		select {
		case <-s.closeCh:
			return
		case <-timer.C:
		}
		s.broadcastHeartbeat()
	}
}

func (s *Server) UpdateHealthCheckConfig(config *HealthCheckConfig) error {
	s.configLock.Lock()
	defer s.configLock.Unlock()
	return s.healthCheck.UpdateConfig(config)
}

func (s *Server) IsHealth() bool {
	return s.healthCheck.IsHealth()
}

func (s *Server) HealthCheckStats() HealthCheckStats {
	stats := s.healthCheck.HealthCheckStats()
	return stats
}

func NewFromSocketFactoryWithPoolSize(socketFactory func() (net.Conn, error), poolSize int) (*Server, error) {
	ret := &Server{
		socketFactory: socketFactory,
		poolCh:        make(chan *conn, poolSize),
		poolSize:      int64(poolSize),
		closeCh:       make(chan struct{}),
		logger:        log.New(os.Stdout, "snserver", log.LstdFlags),
		cntlock:       sync.Mutex{},
		configLock:    sync.RWMutex{},
	}

	healthCheck, err := NewHealthCheckService()
	if err != nil {
		return nil, err
	}
	ret.healthCheck = healthCheck

	go ret.runHeartbeatCo()
	go ret.healthCheck.Run()
	return ret, nil
}

func NewFromSocketFactory(socketFactory func() (net.Conn, error)) (*Server, error) {
	return NewFromSocketFactoryWithPoolSize(socketFactory, DEFAULT_POOL_SIZE)
}

func NewWithPoolSize(addr string, poolSize int) (*Server, error) {
	return NewFromSocketFactoryWithPoolSize(func() (net.Conn, error) {
		return net.Dial("tcp", addr)
	}, poolSize)
}

func New(addr string) (*Server, error) {
	return NewWithPoolSize(addr, DEFAULT_POOL_SIZE)
}

func NewWithPoolSizeWithTimeout(addr string, poolSize int, timeout time.Duration) (*Server, error) {
	return NewFromSocketFactoryWithPoolSize(func() (net.Conn, error) {
		return net.DialTimeout("tcp", addr, timeout)
	}, poolSize)
}

func NewWithTimeout(addr string, timeout time.Duration) (*Server, error) {
	return NewWithPoolSizeWithTimeout(addr, DEFAULT_POOL_SIZE, timeout)
}

func (s *Server) DetectRequestInCtx(dc *detection.DetectionContext) (*detection.Result, error) {
	c, err := s.GetConn()
	if err != nil {
		return nil, err
	}
	defer s.PutConn(c)
	return c.DetectRequestInCtx(dc)
}

func (s *Server) DetectResponseInCtx(dc *detection.DetectionContext) (*detection.Result, error) {
	c, err := s.GetConn()
	if err != nil {
		return nil, misc.ErrorWrap(err, "")
	}
	defer s.PutConn(c)
	return c.DetectResponseInCtx(dc)
}

func (s *Server) Detect(dc *detection.DetectionContext) (*detection.Result, *detection.Result, error) {
	c, err := s.GetConn()
	if err != nil {
		return nil, nil, misc.ErrorWrap(err, "")
	}

	reqResult, rspResult, err := c.Detect(dc)
	if err == nil {
		s.PutConn(c)
	}
	return reqResult, rspResult, err
}

func (s *Server) DetectHttpRequest(req *http.Request) (*detection.Result, error) {
	c, err := s.GetConn()
	if err != nil {
		return nil, err
	}
	defer s.PutConn(c)
	return c.DetectHttpRequest(req)
}

func (s *Server) DetectRequest(req detection.Request) (*detection.Result, error) {
	c, err := s.GetConn()
	if err != nil {
		return nil, err
	}
	defer s.PutConn(c)
	return c.DetectRequest(req)
}

// blocks until all pending detection is completed
func (s *Server) Close() {
	close(s.closeCh)
	for i := int64(0); i < s.count; i++ {
		c, err := s.GetConn()
		if err != nil {
			return
		}
		c.Close()
	}
	s.healthCheck.Close()
}
