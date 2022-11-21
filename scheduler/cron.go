package scheduler

import (
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mylxsw/glacier/log"

	"github.com/mylxsw/glacier/infra"
	"github.com/pkg/errors"
	"github.com/robfig/cron/v3"
)

// JobCreator is a creator for cron job
type JobCreator interface {
	// Add a cron job
	Add(name string, plan string, handler interface{}) error
	// AddAndRunOnServerReady add a cron job, and trigger it immediately when server is ready
	AddAndRunOnServerReady(name string, plan string, handler interface{}) error

	// MustAdd add a cron job
	MustAdd(name string, plan string, handler interface{})
	// MustAddAndRunOnServerReady add a cron job, and trigger it immediately when server is ready
	MustAddAndRunOnServerReady(name string, plan string, handler interface{})
}

// Scheduler is a manager object to manage cron jobs
type Scheduler interface {
	JobCreator
	// Remove remove a cron job
	Remove(name string) error
	// Pause set job status to paused
	Pause(name string) error
	// Continue set job status to continue
	Continue(name string) error
	// Info get job info
	Info(name string) (Job, error)

	// Start cron manager
	Start()
	// Stop cron job manager
	Stop()

	// DistributeLockManager is a setter method for distribute lock manager
	DistributeLockManager(lockManager DistributeLockManager)
}

// DistributeLockManager is a distributed lock manager interface
type DistributeLockManager interface {
	// TryLock try to get lock
	// this method will be called every 60s
	// you should set a ttl for lock since unlock method may be not be called in some case
	TryLock() error
	// TryUnLock try to release the lock
	TryUnLock() error
	// HasLock return whether manager has lock
	HasLock() bool
}

type schedulerImpl struct {
	lock     sync.RWMutex
	resolver infra.Resolver
	cr       *cron.Cron

	distributeLockManager DistributeLockManager

	jobs map[string]*Job
}

// Job is a job object
type Job struct {
	ID      cron.EntryID
	Name    string
	Plan    string
	handler func()
	Paused  bool
}

// Next get execute plan for job
func (job Job) Next(nextNum int) ([]time.Time, error) {
	parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sc, err := parser.Parse(job.Plan)
	if err != nil {
		return nil, err
	}

	results := make([]time.Time, nextNum)
	lastTs := time.Now()
	for i := 0; i < nextNum; i++ {
		lastTs = sc.Next(lastTs)
		results[i] = lastTs
	}

	return results, nil
}

// NewManager create a new Scheduler
func NewManager(resolver infra.Resolver) Scheduler {
	m := schedulerImpl{resolver: resolver, jobs: make(map[string]*Job)}
	resolver.MustResolve(func(cr *cron.Cron) { m.cr = cr })

	return &m
}

func (c *schedulerImpl) DistributeLockManager(lockManager DistributeLockManager) {
	c.distributeLockManager = lockManager
}

func (c *schedulerImpl) MustAddAndRunOnServerReady(name string, plan string, handler interface{}) {
	if err := c.AddAndRunOnServerReady(name, plan, handler); err != nil {
		panic(err)
	}
}

func (c *schedulerImpl) AddAndRunOnServerReady(name string, plan string, handler interface{}) error {
	if err := c.Add(name, plan, handler); err != nil {
		return err
	}

	hh, ok := handler.(JobHandler)
	if ok {
		handler = hh.Handle
	}

	return c.resolver.Resolve(func(hook infra.Hook) {
		hook.OnServerReady(handler)
	})
}

func (c *schedulerImpl) MustAdd(name string, plan string, handler interface{}) {
	if err := c.Add(name, plan, handler); err != nil {
		panic(err)
	}
}

func (c *schedulerImpl) Add(name string, plan string, handler interface{}) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if reg, existed := c.jobs[name]; existed {
		return fmt.Errorf("job with name [%s] already existed: %d | %s", name, reg.ID, reg.Plan)
	}

	hh, ok := handler.(JobHandler)
	if !ok {
		hh = newHandler(handler)
	}

	jobHandler := func() {
		if c.distributeLockManager != nil && !c.distributeLockManager.HasLock() {
			if infra.DEBUG {
				log.Debugf("[glacier] cron job [%s] can not start because it doesn't get the lock", name)
			}
			return
		}

		if infra.DEBUG {
			log.Debugf("[glacier] cron job [%s] running", name)
		}

		startTs := time.Now()
		defer func() {
			if err := recover(); err != nil {
				log.Errorf("[glacier] cron job [%s] stopped with some errors: %v, took %s", name, err, time.Since(startTs))
			} else {
				if infra.DEBUG {
					log.Debugf("[glacier] cron job [%s] stopped, took %s", name, time.Since(startTs))
				}
			}
		}()
		if err := c.resolver.ResolveWithError(hh.Handle); err != nil {
			log.Errorf("[glacier] cron job [%s] failed, Err: %v, Stack: \n%s", name, err, debug.Stack())
		}
	}
	id, err := c.cr.AddFunc(plan, jobHandler)

	if err != nil {
		return errors.Wrap(err, "[glacier] add cron job failed")
	}

	c.jobs[name] = &Job{
		ID:      id,
		Name:    name,
		Plan:    plan,
		handler: jobHandler,
		Paused:  false,
	}

	if infra.DEBUG {
		log.Debugf("[glacier] add job [%s] to scheduler(%s)", name, plan)
	}

	return nil
}

func (c *schedulerImpl) Remove(name string) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	reg, exist := c.jobs[name]
	if !exist {
		return errors.Errorf("[glacier] job with name [%s] not found", name)
	}

	delete(c.jobs, name)
	if !reg.Paused {
		c.cr.Remove(reg.ID)
	}

	if infra.DEBUG {
		log.Debugf("[glacier] remove job [%s] from scheduler", name)
	}

	return nil
}

func (c *schedulerImpl) Pause(name string) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	reg, exist := c.jobs[name]
	if !exist {
		return errors.Errorf("[glacier] job with name [%s] not found", name)
	}

	if reg.Paused {
		return nil
	}

	c.cr.Remove(reg.ID)
	reg.Paused = true

	if infra.DEBUG {
		log.Debugf("[glacier] change job [%s] to paused", name)
	}

	return nil
}

func (c *schedulerImpl) Continue(name string) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	reg, exist := c.jobs[name]
	if !exist {
		return errors.Errorf("[glacier] job with name [%s] not found", name)
	}

	if !reg.Paused {
		return nil
	}

	id, err := c.cr.AddFunc(reg.Plan, reg.handler)
	if err != nil {
		return errors.Wrap(err, "[glacier] change job from paused to continue failed")
	}

	reg.Paused = false
	reg.ID = id

	if infra.DEBUG {
		log.Debugf("[glacier] change job [%s] to continue", name)
	}

	return nil
}

func (c *schedulerImpl) Info(name string) (Job, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	if job, ok := c.jobs[name]; ok {
		return *job, nil
	}

	return Job{}, fmt.Errorf("[glacier] job with name [%s] not found", name)
}

func (c *schedulerImpl) Start() {
	if c.distributeLockManager != nil {
		getDistributeLock := func() {
			if err := c.distributeLockManager.TryLock(); err != nil {
				if infra.WARN {
					log.Warningf("[glacier] try to get distribute lock failed: %v", err)
				}
			}
		}

		getDistributeLock()
		if _, err := c.cr.AddFunc("@every 60s", getDistributeLock); err != nil {
			log.Errorf("[glacier] initialize scheduler failed: can not create distribute lock task: %v", err)
		}
	}

	c.cr.Start()
}

func (c *schedulerImpl) Stop() {
	c.cr.Stop()
	if c.distributeLockManager != nil {
		if err := c.distributeLockManager.TryUnLock(); err != nil {
			if infra.WARN {
				log.Warningf("[glacier] try to release distribute lock failed: %v", err)
			}
		}
	}
}
