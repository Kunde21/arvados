// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package test

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	math_rand "math/rand"
	"regexp"
	"strings"
	"sync"
	"time"

	"git.curoverse.com/arvados.git/lib/cloud"
	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// A StubDriver implements cloud.Driver by setting up local SSH
// servers that do fake command executions.
type StubDriver struct {
	HostKey        ssh.Signer
	AuthorizedKeys []ssh.PublicKey

	// SetupVM, if set, is called upon creation of each new
	// StubVM. This is the caller's opportunity to customize the
	// VM's error rate and other behaviors.
	SetupVM func(*StubVM)

	// StubVM's fake crunch-run uses this Queue to read and update
	// container state.
	Queue *Queue

	// Frequency of artificially introduced errors on calls to
	// Destroy. 0=always succeed, 1=always fail.
	ErrorRateDestroy float64

	// If Create() or Instances() is called too frequently, return
	// rate-limiting errors.
	MinTimeBetweenCreateCalls    time.Duration
	MinTimeBetweenInstancesCalls time.Duration

	// If true, Create and Destroy calls block until Release() is
	// called.
	HoldCloudOps bool

	instanceSets []*StubInstanceSet
	holdCloudOps chan bool
}

// InstanceSet returns a new *StubInstanceSet.
func (sd *StubDriver) InstanceSet(params json.RawMessage, id cloud.InstanceSetID, _ cloud.SharedResourceTags, logger logrus.FieldLogger) (cloud.InstanceSet, error) {
	if sd.holdCloudOps == nil {
		sd.holdCloudOps = make(chan bool)
	}
	sis := StubInstanceSet{
		driver:  sd,
		logger:  logger,
		servers: map[cloud.InstanceID]*StubVM{},
	}
	sd.instanceSets = append(sd.instanceSets, &sis)

	var err error
	if params != nil {
		err = json.Unmarshal(params, &sis)
	}
	return &sis, err
}

// InstanceSets returns all instances that have been created by the
// driver. This can be used to test a component that uses the driver
// but doesn't expose the InstanceSets it has created.
func (sd *StubDriver) InstanceSets() []*StubInstanceSet {
	return sd.instanceSets
}

// ReleaseCloudOps releases n pending Create/Destroy calls. If there
// are fewer than n blocked calls pending, it waits for the rest to
// arrive.
func (sd *StubDriver) ReleaseCloudOps(n int) {
	for i := 0; i < n; i++ {
		<-sd.holdCloudOps
	}
}

type StubInstanceSet struct {
	driver  *StubDriver
	logger  logrus.FieldLogger
	servers map[cloud.InstanceID]*StubVM
	mtx     sync.RWMutex
	stopped bool

	allowCreateCall    time.Time
	allowInstancesCall time.Time
}

func (sis *StubInstanceSet) Create(it arvados.InstanceType, image cloud.ImageID, tags cloud.InstanceTags, cmd cloud.InitCommand, authKey ssh.PublicKey) (cloud.Instance, error) {
	if sis.driver.HoldCloudOps {
		sis.driver.holdCloudOps <- true
	}
	sis.mtx.Lock()
	defer sis.mtx.Unlock()
	if sis.stopped {
		return nil, errors.New("StubInstanceSet: Create called after Stop")
	}
	if sis.allowCreateCall.After(time.Now()) {
		return nil, RateLimitError{sis.allowCreateCall}
	} else {
		sis.allowCreateCall = time.Now().Add(sis.driver.MinTimeBetweenCreateCalls)
	}

	ak := sis.driver.AuthorizedKeys
	if authKey != nil {
		ak = append([]ssh.PublicKey{authKey}, ak...)
	}
	svm := &StubVM{
		sis:          sis,
		id:           cloud.InstanceID(fmt.Sprintf("stub-%s-%x", it.ProviderType, math_rand.Int63())),
		tags:         copyTags(tags),
		providerType: it.ProviderType,
		initCommand:  cmd,
		running:      map[string]int64{},
		killing:      map[string]bool{},
	}
	svm.SSHService = SSHService{
		HostKey:        sis.driver.HostKey,
		AuthorizedUser: "root",
		AuthorizedKeys: ak,
		Exec:           svm.Exec,
	}
	if setup := sis.driver.SetupVM; setup != nil {
		setup(svm)
	}
	sis.servers[svm.id] = svm
	return svm.Instance(), nil
}

func (sis *StubInstanceSet) Instances(cloud.InstanceTags) ([]cloud.Instance, error) {
	sis.mtx.RLock()
	defer sis.mtx.RUnlock()
	if sis.allowInstancesCall.After(time.Now()) {
		return nil, RateLimitError{sis.allowInstancesCall}
	} else {
		sis.allowInstancesCall = time.Now().Add(sis.driver.MinTimeBetweenInstancesCalls)
	}
	var r []cloud.Instance
	for _, ss := range sis.servers {
		r = append(r, ss.Instance())
	}
	return r, nil
}

func (sis *StubInstanceSet) Stop() {
	sis.mtx.Lock()
	defer sis.mtx.Unlock()
	if sis.stopped {
		panic("Stop called twice")
	}
	sis.stopped = true
}

type RateLimitError struct{ Retry time.Time }

func (e RateLimitError) Error() string            { return fmt.Sprintf("rate limited until %s", e.Retry) }
func (e RateLimitError) EarliestRetry() time.Time { return e.Retry }

// StubVM is a fake server that runs an SSH service. It represents a
// VM running in a fake cloud.
//
// Note this is distinct from a stubInstance, which is a snapshot of
// the VM's metadata. Like a VM in a real cloud, a StubVM keeps
// running (and might change IP addresses, shut down, etc.)  without
// updating any stubInstances that have been returned to callers.
type StubVM struct {
	Boot                  time.Time
	Broken                time.Time
	ReportBroken          time.Time
	CrunchRunMissing      bool
	CrunchRunCrashRate    float64
	CrunchRunDetachDelay  time.Duration
	ExecuteContainer      func(arvados.Container) int
	CrashRunningContainer func(arvados.Container)

	sis          *StubInstanceSet
	id           cloud.InstanceID
	tags         cloud.InstanceTags
	initCommand  cloud.InitCommand
	providerType string
	SSHService   SSHService
	running      map[string]int64
	killing      map[string]bool
	lastPID      int64
	sync.Mutex
}

func (svm *StubVM) Instance() stubInstance {
	svm.Lock()
	defer svm.Unlock()
	return stubInstance{
		svm:  svm,
		addr: svm.SSHService.Address(),
		// We deliberately return a cached/stale copy of the
		// real tags here, so that (Instance)Tags() sometimes
		// returns old data after a call to
		// (Instance)SetTags().  This is permitted by the
		// driver interface, and this might help remind
		// callers that they need to tolerate it.
		tags: copyTags(svm.tags),
	}
}

func (svm *StubVM) Exec(env map[string]string, command string, stdin io.Reader, stdout, stderr io.Writer) uint32 {
	stdinData, err := ioutil.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "error reading stdin: %s\n", err)
		return 1
	}
	queue := svm.sis.driver.Queue
	uuid := regexp.MustCompile(`.{5}-dz642-.{15}`).FindString(command)
	if eta := svm.Boot.Sub(time.Now()); eta > 0 {
		fmt.Fprintf(stderr, "stub is booting, ETA %s\n", eta)
		return 1
	}
	if !svm.Broken.IsZero() && svm.Broken.Before(time.Now()) {
		fmt.Fprintf(stderr, "cannot fork\n")
		return 2
	}
	if svm.CrunchRunMissing && strings.Contains(command, "crunch-run") {
		fmt.Fprint(stderr, "crunch-run: command not found\n")
		return 1
	}
	if strings.HasPrefix(command, "crunch-run --detach --stdin-env ") {
		var stdinKV map[string]string
		err := json.Unmarshal(stdinData, &stdinKV)
		if err != nil {
			fmt.Fprintf(stderr, "unmarshal stdin: %s (stdin was: %q)\n", err, stdinData)
			return 1
		}
		for _, name := range []string{"ARVADOS_API_HOST", "ARVADOS_API_TOKEN"} {
			if stdinKV[name] == "" {
				fmt.Fprintf(stderr, "%s env var missing from stdin %q\n", name, stdinData)
				return 1
			}
		}
		svm.Lock()
		svm.lastPID++
		pid := svm.lastPID
		svm.running[uuid] = pid
		svm.Unlock()
		time.Sleep(svm.CrunchRunDetachDelay)
		fmt.Fprintf(stderr, "starting %s\n", uuid)
		logger := svm.sis.logger.WithFields(logrus.Fields{
			"Instance":      svm.id,
			"ContainerUUID": uuid,
			"PID":           pid,
		})
		logger.Printf("[test] starting crunch-run stub")
		go func() {
			crashluck := math_rand.Float64()
			ctr, ok := queue.Get(uuid)
			if !ok {
				logger.Print("[test] container not in queue")
				return
			}

			defer func() {
				if ctr.State == arvados.ContainerStateRunning && svm.CrashRunningContainer != nil {
					svm.CrashRunningContainer(ctr)
				}
			}()

			if crashluck > svm.CrunchRunCrashRate/2 {
				time.Sleep(time.Duration(math_rand.Float64()*20) * time.Millisecond)
				ctr.State = arvados.ContainerStateRunning
				if !queue.Notify(ctr) {
					ctr, _ = queue.Get(uuid)
					logger.Print("[test] erroring out because state=Running update was rejected")
					return
				}
			}

			time.Sleep(time.Duration(math_rand.Float64()*20) * time.Millisecond)

			svm.Lock()
			defer svm.Unlock()
			if svm.running[uuid] != pid {
				logger.Print("[test] container was killed")
				return
			}
			delete(svm.running, uuid)

			if crashluck < svm.CrunchRunCrashRate {
				logger.WithField("State", ctr.State).Print("[test] crashing crunch-run stub")
			} else {
				if svm.ExecuteContainer != nil {
					ctr.ExitCode = svm.ExecuteContainer(ctr)
				}
				logger.WithField("ExitCode", ctr.ExitCode).Print("[test] exiting crunch-run stub")
				ctr.State = arvados.ContainerStateComplete
				go queue.Notify(ctr)
			}
		}()
		return 0
	}
	if command == "crunch-run --list" {
		svm.Lock()
		defer svm.Unlock()
		for uuid := range svm.running {
			fmt.Fprintf(stdout, "%s\n", uuid)
		}
		if !svm.ReportBroken.IsZero() && svm.ReportBroken.Before(time.Now()) {
			fmt.Fprintln(stdout, "broken")
		}
		return 0
	}
	if strings.HasPrefix(command, "crunch-run --kill ") {
		svm.Lock()
		pid, running := svm.running[uuid]
		if running && !svm.killing[uuid] {
			svm.killing[uuid] = true
			go func() {
				time.Sleep(time.Duration(math_rand.Float64()*30) * time.Millisecond)
				svm.Lock()
				defer svm.Unlock()
				if svm.running[uuid] == pid {
					// Kill only if the running entry
					// hasn't since been killed and
					// replaced with a different one.
					delete(svm.running, uuid)
				}
				delete(svm.killing, uuid)
			}()
			svm.Unlock()
			time.Sleep(time.Duration(math_rand.Float64()*2) * time.Millisecond)
			svm.Lock()
			_, running = svm.running[uuid]
		}
		svm.Unlock()
		if running {
			fmt.Fprintf(stderr, "%s: container is running\n", uuid)
			return 1
		} else {
			fmt.Fprintf(stderr, "%s: container is not running\n", uuid)
			return 0
		}
	}
	if command == "true" {
		return 0
	}
	fmt.Fprintf(stderr, "%q: command not found", command)
	return 1
}

type stubInstance struct {
	svm  *StubVM
	addr string
	tags cloud.InstanceTags
}

func (si stubInstance) ID() cloud.InstanceID {
	return si.svm.id
}

func (si stubInstance) Address() string {
	return si.addr
}

func (si stubInstance) RemoteUser() string {
	return si.svm.SSHService.AuthorizedUser
}

func (si stubInstance) Destroy() error {
	sis := si.svm.sis
	if sis.driver.HoldCloudOps {
		sis.driver.holdCloudOps <- true
	}
	if math_rand.Float64() < si.svm.sis.driver.ErrorRateDestroy {
		return errors.New("instance could not be destroyed")
	}
	si.svm.SSHService.Close()
	sis.mtx.Lock()
	defer sis.mtx.Unlock()
	delete(sis.servers, si.svm.id)
	return nil
}

func (si stubInstance) ProviderType() string {
	return si.svm.providerType
}

func (si stubInstance) SetTags(tags cloud.InstanceTags) error {
	tags = copyTags(tags)
	svm := si.svm
	go func() {
		svm.Lock()
		defer svm.Unlock()
		svm.tags = tags
	}()
	return nil
}

func (si stubInstance) Tags() cloud.InstanceTags {
	// Return a copy to ensure a caller can't change our saved
	// tags just by writing to the returned map.
	return copyTags(si.tags)
}

func (si stubInstance) String() string {
	return string(si.svm.id)
}

func (si stubInstance) VerifyHostKey(key ssh.PublicKey, client *ssh.Client) error {
	buf := make([]byte, 512)
	_, err := io.ReadFull(rand.Reader, buf)
	if err != nil {
		return err
	}
	sig, err := si.svm.sis.driver.HostKey.Sign(rand.Reader, buf)
	if err != nil {
		return err
	}
	return key.Verify(buf, sig)
}

func copyTags(src cloud.InstanceTags) cloud.InstanceTags {
	dst := cloud.InstanceTags{}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
