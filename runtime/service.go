// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"context"
	"io"
	"io/ioutil"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/fifo"
	"github.com/containerd/ttrpc"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/mdlayher/vsock"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/firecracker-microvm/firecracker-containerd/internal"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
)

const (
	defaultVsockPort     = 10789
	supportedMountFSType = "ext4"
)

// implements shimapi
type service struct {
	server  *ttrpc.Server
	id      string
	publish events.Publisher

	agentStarted bool
	agentClient  taskAPI.TaskService
	config       *Config
	machine      *firecracker.Machine
	machineCID   uint32
	ctx          context.Context
	cancel       context.CancelFunc
}

var (
	_       = (taskAPI.TaskService)(&service{})
	sysCall = syscall.Syscall
)

// Matches type Init func(..).. defined https://github.com/containerd/containerd/blob/master/runtime/v2/shim/shim.go#L47
func NewService(ctx context.Context, id string, publisher events.Publisher) (shim.Shim, error) {
	server, err := newServer()
	if err != nil {
		return nil, err
	}

	config, err := LoadConfig("")
	if err != nil {
		return nil, err
	}

	if !config.Debug {
		// Check if containerd is running in debug mode
		opts := ctx.Value(shim.OptsKey{}).(shim.Opts)
		config.Debug = opts.Debug
	}

	s := &service{
		server:  server,
		id:      id,
		publish: publisher,
		config:  config,
	}

	return s, nil
}

func (s *service) StartShim(ctx context.Context, id, containerdBinary, containerdAddress string) (string, error) {
	cmd, err := s.newCommand(ctx, containerdBinary, containerdAddress)
	if err != nil {
		return "", err
	}

	address, err := shim.SocketAddress(ctx, id)
	if err != nil {
		return "", err
	}

	socket, err := shim.NewSocket(address)
	if err != nil {
		return "", err
	}

	defer socket.Close()

	f, err := socket.File()
	if err != nil {
		return "", err
	}

	defer f.Close()

	cmd.ExtraFiles = append(cmd.ExtraFiles, f)

	if err := cmd.Start(); err != nil {
		return "", err
	}

	defer func() {
		if err != nil {
			cmd.Process.Kill()
		}
	}()

	// make sure to wait after start
	go cmd.Wait()
	if err := shim.WritePidFile("shim.pid", cmd.Process.Pid); err != nil {
		return "", err
	}

	if err := shim.WriteAddress("address", address); err != nil {
		return "", err
	}

	if err := shim.SetScore(cmd.Process.Pid); err != nil {
		return "", errors.Wrap(err, "failed to set OOM Score on shim")
	}

	return address, nil
}

func (s *service) Create(ctx context.Context, request *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	log.G(ctx).WithFields(logrus.Fields{
		"id":         request.ID,
		"bundle":     request.Bundle,
		"terminal":   request.Terminal,
		"stdin":      request.Stdin,
		"stdout":     request.Stdout,
		"stderr":     request.Stderr,
		"checkpoint": request.Checkpoint,
	}).Debug("creating task")

	// TODO: should there be a lock here
	if !s.agentStarted {
		client, err := s.startVM(ctx, request)
		if err != nil {
			log.G(ctx).WithError(err).Error("failed to start VM")
			return nil, err
		}

		s.agentClient = client
		s.agentStarted = true
	}

	log.G(ctx).Infof("creating task '%s'", request.ID)

	// Generate new anyData with bundle/config.json packed inside
	anyData, err := packBundle(filepath.Join(request.Bundle, "config.json"), request.Options)
	if err != nil {
		return nil, err
	}

	request.Options = anyData

	resp, err := s.agentClient.Create(ctx, request)
	if err != nil {
		log.G(ctx).WithError(err).Error("create failed")
		return nil, err
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	go s.proxyStdio(s.ctx, request.Stdin, request.Stdout, request.Stderr, s.machineCID)
	log.G(ctx).Infof("successfully created task with pid %d", resp.Pid)
	return resp, nil
}

func (s *service) Start(ctx context.Context, req *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "exec_id": req.ExecID}).Debug("start")
	resp, err := s.agentClient.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	// TODO: Do we need to cancel this at some point?
	go s.monitorState(s.ctx, req.ID, req.ExecID, resp.Pid)

	return resp, nil
}

func (s *service) monitorState(ctx context.Context, id, execID string, pid uint32) {
	ticker := time.NewTicker(time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			//make a state request
			req := &taskAPI.StateRequest{
				ID:     id,
				ExecID: execID,
			}
			resp, err := s.agentClient.State(ctx, req)
			if err != nil {
				log.G(ctx).WithError(err).Error("error monitoring state")
				continue
			}
			// if ending state, stop vm and break
			if resp.Status == task.StatusStopped {
				s.publish.Publish(ctx, runtime.TaskExitEventTopic, &eventstypes.TaskExit{
					ContainerID: s.id,
					ID:          s.id,
					Pid:         pid,
					ExitStatus:  resp.ExitStatus,
					ExitedAt:    time.Now(),
				})
				s.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: id})
				s.server.Close()
				return
			}
		}

	}
}

func (s *service) proxyStdio(ctx context.Context, stdin, stdout, stderr string, CID uint32) {
	go proxyIO(ctx, stdin, CID, internal.StdinPort, true)
	go proxyIO(ctx, stdout, CID, internal.StdoutPort, false)
	go proxyIO(ctx, stderr, CID, internal.StderrPort, false)
}

func proxyIO(ctx context.Context, path string, CID, port uint32, in bool) {
	if path == "" {
		return
	}
	log.G(ctx).Debug("setting up IO for " + path)
	f, err := fifo.OpenFifo(ctx, path, syscall.O_RDWR|syscall.O_NONBLOCK, 0700)
	if err != nil {
		log.G(ctx).WithError(err).Error("error opening fifo")
		return
	}
	conn, err := vsock.Dial(CID, port)
	if err != nil {
		log.G(ctx).WithError(err).Error("unable to dial agent vsock")
		f.Close()
		return
	}
	go func() {
		<-ctx.Done()
		conn.Close()
		f.Close()
	}()
	log.G(ctx).Debug("begin copying io")
	buf := make([]byte, internal.DefaultBufferSize)
	if in {
		_, err = io.CopyBuffer(conn, f, buf)
	} else {
		_, err = io.CopyBuffer(f, conn, buf)
	}
	if err != nil {
		log.G(ctx).WithError(err).Error("error with stdio")
	}
}

// Delete the initial process and container
func (s *service) Delete(ctx context.Context, req *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "exec_id": req.ExecID}).Debug("delete")
	resp, err := s.agentClient.Delete(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, req *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "exec_id": req.ExecID}).Debug("exec")
	resp, err := s.agentClient.Exec(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, req *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "exec_id": req.ExecID}).Debug("resize_pty")
	resp, err := s.agentClient.ResizePty(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, req *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "exec_id": req.ExecID}).Debug("state")
	resp, err := s.agentClient.State(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Pause the container
func (s *service) Pause(ctx context.Context, req *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithField("id", req.ID).Debug("pause")
	resp, err := s.agentClient.Pause(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Resume the container
func (s *service) Resume(ctx context.Context, req *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithField("id", req.ID).Debug("resume")
	resp, err := s.agentClient.Resume(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, req *taskAPI.KillRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "exec_id": req.ExecID}).Debug("kill")
	// right now we want to kill vm always when kill is called
	// may not be true in multi-container vm
	defer func() {
		log.G(ctx).Debug("Stopping VM during kill")
		if err := s.stopVM(); err != nil {
			log.G(ctx).WithError(err).Error("failed to stop VM")
		}
	}()
	resp, err := s.agentClient.Kill(ctx, req)
	if err != nil {
		return nil, err
	}
	s.cancel()
	return resp, nil
}

// Pids returns all pids inside the container
func (s *service) Pids(ctx context.Context, req *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	log.G(ctx).WithField("id", req.ID).Debug("pids")
	resp, err := s.agentClient.Pids(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, req *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "exec_id": req.ExecID}).Debug("close_io")
	resp, err := s.agentClient.CloseIO(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, req *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "path": req.Path}).Info("checkpoint")
	resp, err := s.agentClient.Checkpoint(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Connect returns shim information such as the shim's pid
func (s *service) Connect(ctx context.Context, req *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	log.G(ctx).WithField("id", req.ID).Debug("connect")
	resp, err := s.agentClient.Connect(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *service) Shutdown(ctx context.Context, req *taskAPI.ShutdownRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "now": req.Now}).Debug("shutdown")
	if _, err := s.agentClient.Shutdown(ctx, req); err != nil {
		log.G(ctx).WithError(err).Error("failed to shutdown agent")
	}
	log.G(ctx).Debug("stopping VM")
	if err := s.stopVM(); err != nil {
		log.G(ctx).WithError(err).Error("failed to stop VM")
		return nil, err
	}
	s.cancel()
	// Exit to avoid 'zombie' shim processes
	defer os.Exit(0)
	log.G(ctx).Debug("stopping runtime")
	return &ptypes.Empty{}, nil
}

func (s *service) Stats(ctx context.Context, req *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	log.G(ctx).WithField("id", req.ID).Debug("stats")
	resp, err := s.agentClient.Stats(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Update a running container
func (s *service) Update(ctx context.Context, req *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithField("id", req.ID).Debug("update")
	resp, err := s.agentClient.Update(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, req *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	log.G(ctx).WithFields(logrus.Fields{"id": req.ID, "exec_id": req.ExecID}).Debug("wait")
	resp, err := s.agentClient.Wait(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *service) Cleanup(ctx context.Context) (*taskAPI.DeleteResponse, error) {
	log.G(ctx).Debug("cleanup")
	// Destroy VM/etc here?
	// copied from runcs impl, nothing to cleanup atm
	return &taskAPI.DeleteResponse{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + uint32(unix.SIGKILL),
	}, nil
}

func (s *service) newCommand(ctx context.Context, containerdBinary, containerdAddress string) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	self, err := os.Executable()
	if err != nil {
		return nil, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	args := []string{
		"-namespace", ns,
		"-address", containerdAddress,
		"-publish-binary", containerdBinary,
	}

	if s.config.Debug {
		args = append(args, "-debug")
	}

	cmd := exec.Command(self, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=2")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	return cmd, nil
}

func newServer() (*ttrpc.Server, error) {
	return ttrpc.NewServer(ttrpc.WithServerHandshaker(ttrpc.UnixSocketRequireSameUser()))
}

func dialVsock(ctx context.Context, contextID uint32, port uint32) (net.Conn, error) {
	// VM should start within 200ms, vsock dial will make retries at 100ms, 200ms, 400ms, 800ms and 1.6s
	const (
		retryCount      = 5
		initialDelay    = 100 * time.Millisecond
		delayMultiplier = 2
	)

	var lastErr error
	var currentDelay = initialDelay
	for i := 1; i <= retryCount; i++ {
		conn, err := vsock.Dial(contextID, port)
		if err == nil {
			log.G(ctx).WithField("connection", conn).Debug("Dial succeeded")
			return conn, nil
		}

		log.G(ctx).WithError(err).Warnf("vsock dial failed (attempt %d of %d), will retry in %s", i, retryCount, currentDelay)
		time.Sleep(currentDelay)

		lastErr = err
		currentDelay *= delayMultiplier
	}

	log.G(ctx).WithError(lastErr).WithFields(logrus.Fields{"context_id": contextID, "port": port}).Error("vsock dial failed")
	return nil, lastErr
}

// findNextAvailableVsockCID finds first available vsock context ID.
// It uses VHOST_VSOCK_SET_GUEST_CID ioctl which allows some CID ranges to be statically reserved in advance.
// The ioctl fails with EADDRINUSE if cid is already taken and with EINVAL if the CID is invalid.
// Taken from https://bugzilla.redhat.com/show_bug.cgi?id=1291851
func findNextAvailableVsockCID(ctx context.Context) (uint32, error) {
	const (
		// Corresponds to VHOST_VSOCK_SET_GUEST_CID in vhost.h
		ioctlVsockSetGuestCID = uintptr(0x4008AF60)
		// 0, 1 and 2 are reserved CIDs, see http://man7.org/linux/man-pages/man7/vsock.7.html
		startCID        = 3
		maxCID          = math.MaxUint32
		vsockDevicePath = "/dev/vhost-vsock"
	)

	file, err := os.OpenFile(vsockDevicePath, syscall.O_RDWR, 0666)
	if err != nil {
		return 0, errors.Wrap(err, "failed to open vsock device")
	}

	defer file.Close()

	for contextID := startCID; contextID < maxCID; contextID++ {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
			cid := contextID
			_, _, err = sysCall(
				unix.SYS_IOCTL,
				file.Fd(),
				ioctlVsockSetGuestCID,
				uintptr(unsafe.Pointer(&cid)))

			switch err {
			case unix.Errno(0):
				return uint32(contextID), nil
			case unix.EADDRINUSE:
				// ID is already taken, try next one
				continue
			default:
				// Fail if we get an error we don't expect
				return 0, err
			}
		}
	}

	return 0, errors.New("couldn't find any available vsock context id")
}

func (s *service) startVM(ctx context.Context, request *taskAPI.CreateTaskRequest) (taskAPI.TaskService, error) {
	log.G(ctx).Info("starting VM")

	cid, err := findNextAvailableVsockCID(ctx)
	if err != nil {
		return nil, err
	}

	cfg := firecracker.Config{
		SocketPath:      s.config.SocketPath,
		VsockDevices:    []firecracker.VsockDevice{{Path: "root", CID: cid}},
		KernelImagePath: s.config.KernelImagePath,
		KernelArgs:      s.config.KernelArgs,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:   int64(s.config.CPUCount),
			CPUTemplate: models.CPUTemplate(s.config.CPUTemplate),
			MemSizeMib:  256,
		},
		LogFifo:     s.config.LogFifo,
		LogLevel:    s.config.LogLevel,
		MetricsFifo: s.config.MetricsFifo,
		Debug:       s.config.Debug,
	}

	idx := strconv.Itoa(1)
	cfg.Drives = append(cfg.Drives,
		models.Drive{
			DriveID:      &idx,
			PathOnHost:   &s.config.RootDrive,
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		})

	// Attach block devices passed from snapshotter
	for i, mnt := range request.Rootfs {
		if mnt.Type != supportedMountFSType {
			return nil, errors.Errorf("unsupported mount type '%s', expected '%s'", mnt.Type, supportedMountFSType)
		}
		idx := strconv.Itoa(i + 2)
		cfg.Drives = append(cfg.Drives,
			models.Drive{
				DriveID:      &idx,
				PathOnHost:   firecracker.String(mnt.Source),
				IsRootDevice: firecracker.Bool(false),
				IsReadOnly:   firecracker.Bool(false),
			})
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(s.config.FirecrackerBinaryPath).
		WithSocketPath(s.config.SocketPath).
		Build(ctx)
	machineOpts := []firecracker.Opt{
		firecracker.WithProcessRunner(cmd),
	}

	vmmCtx, vmmCancel := context.WithCancel(context.Background())
	defer vmmCancel()
	s.machine, err = firecracker.NewMachine(vmmCtx, cfg, machineOpts...)
	if err != nil {
		return nil, err
	}
	s.machineCID = cid

	log.G(ctx).Info("starting instance")
	if err := s.machine.Start(vmmCtx); err != nil {
		return nil, err
	}

	log.G(ctx).Info("calling agent")
	conn, err := dialVsock(ctx, cid, defaultVsockPort)
	if err != nil {
		s.stopVM()
		return nil, err
	}

	log.G(ctx).Info("creating clients")
	rpcClient := ttrpc.NewClient(conn)
	rpcClient.OnClose(func() { conn.Close() })
	apiClient := taskAPI.NewTaskClient(rpcClient)

	return apiClient, nil
}

func (s *service) stopVM() error {
	return s.machine.StopVMM()
}

func packBundle(path string, options *ptypes.Any) (*ptypes.Any, error) {
	// Add the bundle/config.json to the request so it can be recreated
	// inside the vm:
	// Read bundle json
	jsonBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var opts *ptypes.Any
	if options != nil {
		// Copy values of existing options over
		valCopy := make([]byte, len(options.Value))
		copy(valCopy, options.Value)
		opts = &ptypes.Any{
			TypeUrl: options.TypeUrl,
			Value:   valCopy,
		}
	}
	// add it to a type
	// Convert to any
	extraData := &proto.ExtraData{
		JsonSpec:    jsonBytes,
		RuncOptions: opts,
	}
	return ptypes.MarshalAny(extraData)
}
