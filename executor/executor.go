/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package executor

import (
	"fmt"
	"os"
	"time"

	"code.google.com/p/go-uuid/uuid"
	"code.google.com/p/gogoprotobuf/proto"
	log "github.com/golang/glog"
	"github.com/mesos/mesos-go/mesosproto"
	"github.com/mesos/mesos-go/mesosutil"
	"github.com/mesos/mesos-go/messenger"
	"github.com/mesos/mesos-go/upid"
)

// MesosExecutorDriver is a implementation of the ExecutorDriver.
type MesosExecutorDriver struct {
	self            *upid.UPID
	exec            Executor
	stopCh          chan struct{}
	destroyCh       chan struct{}
	stopped         bool
	status          mesosproto.Status
	messenger       messenger.Messenger
	slaveUPID       *upid.UPID
	slaveID         *mesosproto.SlaveID
	frameworkID     *mesosproto.FrameworkID
	executorID      *mesosproto.ExecutorID
	workDir         string
	connected       bool
	connection      uuid.UUID
	local           bool   // TODO(yifan): Not used yet.
	directory       string // TODO(yifan): Not used yet.
	checkpoint      bool
	recoveryTimeout time.Duration
	updates         map[string]*mesosproto.StatusUpdate // Key is a UUID string. TODO(yifan): Not used yet.
	tasks           map[string]*mesosproto.TaskInfo     // Key is a UUID string. TODO(yifan): Not used yet.
}

// NewMesosExecutorDriver creates a new mesos executor driver.
func NewMesosExecutorDriver(exec Executor) (*MesosExecutorDriver, error) {
	if exec == nil {
		msg := "Executor callback interface cannot be nil."
		log.Errorln(msg)
		return nil, fmt.Errorf(msg)
	}

	driver := &MesosExecutorDriver{
		exec:      exec,
		status:    mesosproto.Status_DRIVER_NOT_STARTED,
		destroyCh: make(chan struct{}),
		stopped:   true,
		updates:   make(map[string]*mesosproto.StatusUpdate),
		tasks:     make(map[string]*mesosproto.TaskInfo),
		workDir:   ".",
	}
	// TODO(yifan): Set executor cnt.
	driver.messenger = messenger.NewMesosMessenger(&upid.UPID{ID: "executor(1)"})
	if err := driver.init(); err != nil {
		log.Errorf("Failed to initialize the driver: %v\n", err)
		return nil, err
	}
	return driver, nil
}

// init initializes the driver.
func (driver *MesosExecutorDriver) init() error {
	log.Infof("Init mesos executor driver\n")
	log.Infof("Version: %v\n", mesosutil.MesosVersion)

	// Parse environments.
	if err := driver.parseEnviroments(); err != nil {
		log.Errorf("Failed to parse environments: %v\n", err)
		return err
	}

	// Install handlers.
	driver.messenger.Install(driver.registered, &mesosproto.ExecutorRegisteredMessage{})
	driver.messenger.Install(driver.reregistered, &mesosproto.ExecutorReregisteredMessage{})
	driver.messenger.Install(driver.reconnect, &mesosproto.ReconnectExecutorMessage{})
	driver.messenger.Install(driver.runTask, &mesosproto.RunTaskMessage{})
	driver.messenger.Install(driver.killTask, &mesosproto.KillTaskMessage{})
	driver.messenger.Install(driver.statusUpdateAcknowledgement, &mesosproto.StatusUpdateAcknowledgementMessage{})
	driver.messenger.Install(driver.frameworkMessage, &mesosproto.FrameworkToExecutorMessage{})
	driver.messenger.Install(driver.shutdown, &mesosproto.ShutdownExecutorMessage{})
	driver.messenger.Install(driver.frameworkError, &mesosproto.FrameworkErrorMessage{})
	return nil
}

func (driver *MesosExecutorDriver) parseEnviroments() error {
	var value string

	value = os.Getenv("MESOS_LOCAL")
	if len(value) > 0 {
		driver.local = true
	}

	value = os.Getenv("MESOS_SLAVE_PID")
	if len(value) == 0 {
		return fmt.Errorf("Cannot find MESOS_SLAVE_PID in the environment")
	}
	upid, err := upid.Parse(value)
	if err != nil {
		log.Errorf("Cannot parse UPID %v\n", err)
		return err
	}
	driver.slaveUPID = upid

	value = os.Getenv("MESOS_SLAVE_ID")
	driver.slaveID = &mesosproto.SlaveID{Value: proto.String(value)}

	value = os.Getenv("MESOS_FRAMEWORK_ID")
	driver.frameworkID = &mesosproto.FrameworkID{Value: proto.String(value)}

	value = os.Getenv("MESOS_EXECUTOR_ID")
	driver.executorID = &mesosproto.ExecutorID{Value: proto.String(value)}

	value = os.Getenv("MESOS_DIRECTORY")
	if len(value) > 0 {
		driver.workDir = value
	}

	value = os.Getenv("MESOS_CHECKPOINT")
	if value == "1" {
		driver.checkpoint = true
	}
	// TODO(yifan): Parse the duration. For now just use default.
	return nil
}

// --------------------- Message Handlers --------------------- //

func (driver *MesosExecutorDriver) registered(from *upid.UPID, pbMsg proto.Message) {
	log.Infoln("Executor driver registered")

	msg := pbMsg.(*mesosproto.ExecutorRegisteredMessage)
	slaveID := msg.GetSlaveId()
	executorInfo := msg.GetExecutorInfo()
	frameworkInfo := msg.GetFrameworkInfo()
	slaveInfo := msg.GetSlaveInfo()

	if driver.stopped {
		log.Infof("Ignoring registered message from slave %v, because the driver is stopped!\n", slaveID)
		return
	}

	log.Infof("Registered on slave %v\n", slaveID)
	driver.connected = true
	driver.connection = uuid.NewUUID()
	driver.exec.Registered(driver, executorInfo, frameworkInfo, slaveInfo)
}

func (driver *MesosExecutorDriver) reregistered(from *upid.UPID, pbMsg proto.Message) {
	log.Infoln("Executor driver reregistered")

	msg := pbMsg.(*mesosproto.ExecutorReregisteredMessage)
	slaveID := msg.GetSlaveId()
	slaveInfo := msg.GetSlaveInfo()

	if driver.stopped {
		log.Infof("Ignoring re-registered message from slave %v, because the driver is stopped!\n", slaveID)
		return
	}

	log.Infof("Re-registered on slave %v\n", slaveID)
	driver.connected = true
	driver.connection = uuid.NewUUID()
	driver.exec.Reregistered(driver, slaveInfo)
}

func (driver *MesosExecutorDriver) reconnect(from *upid.UPID, pbMsg proto.Message) {
	log.Infoln("Executor driver reconnect")

	msg := pbMsg.(*mesosproto.ReconnectExecutorMessage)
	slaveID := msg.GetSlaveId()

	if driver.stopped {
		log.Infof("Ignoring reconnect message from slave %v, because the driver is stopped!\n", slaveID)
		return
	}

	log.Infof("Received reconnect request from slave %v\n", slaveID)
	driver.slaveUPID = from

	message := &mesosproto.ReregisterExecutorMessage{
		ExecutorId:  driver.executorID,
		FrameworkId: driver.frameworkID,
	}
	// Send all unacknowledged updates.
	for _, u := range driver.updates {
		message.Updates = append(message.Updates, u)
	}
	// Send all unacknowledged tasks.
	for _, t := range driver.tasks {
		message.Tasks = append(message.Tasks, t)
	}
	// Send the message.
	if err := driver.messenger.Send(driver.slaveUPID, message); err != nil {
		log.Errorf("Failed to send %v: %v\n")
	}
}

func (driver *MesosExecutorDriver) runTask(from *upid.UPID, pbMsg proto.Message) {
	log.Infoln("Executor driver runTask")

	msg := pbMsg.(*mesosproto.RunTaskMessage)
	task := msg.GetTask()
	taskID := task.GetTaskId()

	if driver.stopped {
		log.Infof("Ignoring run task message for task %v because the driver is stopped!\n", taskID)
		return
	}
	if _, ok := driver.tasks[taskID.String()]; ok {
		log.Fatalf("Unexpected duplicate task %v\n", taskID)
	}

	log.Infof("Executor asked to run task '%v'\n", taskID)
	driver.tasks[taskID.String()] = task
	driver.exec.LaunchTask(driver, task)
}

func (driver *MesosExecutorDriver) killTask(from *upid.UPID, pbMsg proto.Message) {
	log.Infoln("Executor driver killTask")

	msg := pbMsg.(*mesosproto.KillTaskMessage)
	taskID := msg.GetTaskId()

	if driver.stopped {
		log.Infof("Ignoring kill task message for task %v, because the driver is stopped!\n", taskID)
		return
	}

	log.Infof("Executor driver is asked to kill task '%v'\n", taskID)
	driver.exec.KillTask(driver, taskID)
}

func (driver *MesosExecutorDriver) statusUpdateAcknowledgement(from *upid.UPID, pbMsg proto.Message) {
	log.Infoln("Executor statusUpdateAcknowledgement")

	msg := pbMsg.(*mesosproto.StatusUpdateAcknowledgementMessage)
	log.Infof("Receiving status update acknowledgement %v", msg)

	frameworkID := msg.GetFrameworkId()
	taskID := msg.GetTaskId()
	uuid := uuid.UUID(msg.GetUuid())

	if driver.stopped {
		log.Infof("Ignoring status update acknowledgement %v for task %v of framework %v because the driver is stopped!\n",
			uuid, taskID, frameworkID)
	}

	// Remove the corresponding update.
	delete(driver.updates, uuid.String())
	// Remove the corresponding task.
	delete(driver.tasks, taskID.String())
}

func (driver *MesosExecutorDriver) frameworkMessage(from *upid.UPID, pbMsg proto.Message) {
	log.Infoln("Executor driver received frameworkMessage")

	msg := pbMsg.(*mesosproto.FrameworkToExecutorMessage)
	data := msg.GetData()

	if driver.stopped {
		log.Infof("Ignoring framework message because the driver is stopped!\n")
		return
	}

	log.Infof("Executor driver receives framework message\n")
	driver.exec.FrameworkMessage(driver, string(data))
}

func (driver *MesosExecutorDriver) shutdown(from *upid.UPID, pbMsg proto.Message) {
	log.Infoln("Executor driver received shutdown")

	_, ok := pbMsg.(*mesosproto.ShutdownExecutorMessage)
	if !ok {
		panic("Not a ShutdownExecutorMessage! This should not happen")
	}

	if driver.stopped {
		log.Infof("Ignoring shutdown message because the driver is stopped!\n")
		return
	}

	log.Infof("Executor driver is asked to shutdown\n")

	if !driver.local {
		// TODO(yifan): go kill.
	}
	driver.exec.Shutdown(driver)
	// driver.Stop() will cause process to eventually stop.
	driver.Stop()
}

func (driver *MesosExecutorDriver) frameworkError(from *upid.UPID, pbMsg proto.Message) {
	log.Infoln("Executor driver received error")

	msg := pbMsg.(*mesosproto.FrameworkErrorMessage)
	driver.exec.Error(driver, msg.GetMessage())
}

// ------------------------ Driver Implementation ----------------- //

// Start starts the driver by sending a 'startEvent' to the event loop, and
// receives the result from the response channel.
func (driver *MesosExecutorDriver) Start() (mesosproto.Status, error) {
	log.Infoln("Starting the executor driver")

	if driver.status != mesosproto.Status_DRIVER_NOT_STARTED {
		return driver.status, nil
	}

	driver.status = mesosproto.Status_DRIVER_NOT_STARTED
	driver.stopped = true

	// Start the messenger.
	if err := driver.messenger.Start(); err != nil {
		log.Errorf("Failed to start executor: %v\n", err)
		return driver.status, err
	}

	driver.self = driver.messenger.UPID()
	// Set status.
	driver.stopped = false
	driver.stopCh = make(chan struct{})
	driver.status = mesosproto.Status_DRIVER_RUNNING
	log.Infoln("Mesos executor is running with PID=", driver.self.String())

	// Register with slave.
	log.V(3).Infoln("Sending Executor registration")
	message := &mesosproto.RegisterExecutorMessage{
		FrameworkId: driver.frameworkID,
		ExecutorId:  driver.executorID,
	}

	if err := driver.messenger.Send(driver.slaveUPID, message); err != nil {
		log.Errorf("Stopping the executor, failed to send %v: %v\n", message, err)
		stat, err0 := driver.Stop()
		if err0 != nil {
			log.Errorf("Failed to stop executor %v: %v\n", message, err)
			return stat, err0
		}
		return stat, err
	}

	return driver.status, nil
}

// Stop stops the driver by sending a 'stopEvent' to the event loop, and
// receives the result from the response channel.
func (driver *MesosExecutorDriver) Stop() (mesosproto.Status, error) {
	log.Infoln("Stopping the executor driver")
	if driver.status != mesosproto.Status_DRIVER_RUNNING {
		return driver.status, nil
	}
	stopStat := mesosproto.Status_DRIVER_STOPPED
	return stopStat, driver.stop(stopStat)
}

// internal function for stopping the driver and set reason for stopping
// Note that messages inflight or queued will not be processed.
func (driver *MesosExecutorDriver) stop(stopStatus mesosproto.Status) error {
	err := driver.messenger.Stop()
	defer close(driver.destroyCh)
	defer close(driver.stopCh)

	driver.status = stopStatus
	driver.stopped = true

	if err != nil {
		return err
	}

	return nil
}

// Abort aborts the driver by sending an 'abortEvent' to the event loop, and
// receives the result from the response channel.
func (driver *MesosExecutorDriver) Abort() (mesosproto.Status, error) {
	if driver.status != mesosproto.Status_DRIVER_RUNNING {
		return driver.status, nil
	}

	log.Infoln("Aborting the executor driver")
	abortStat := mesosproto.Status_DRIVER_ABORTED
	return abortStat, driver.stop(abortStat)
}

// Join waits for the driver by sending a 'joinEvent' to the event loop, and wait
// on a channel for the notification of driver termination.
func (driver *MesosExecutorDriver) Join() (mesosproto.Status, error) {
	log.Infoln("Waiting for the executor driver to stop")
	if driver.status != mesosproto.Status_DRIVER_RUNNING {
		return driver.status, nil
	}
	<-driver.stopCh // wait for stop signal
	return driver.status, nil
}

func (driver *MesosExecutorDriver) Run() (mesosproto.Status, error) {
	stat, err := driver.Start()

	if err != nil {
		return driver.Stop()
	}

	if stat != mesosproto.Status_DRIVER_RUNNING {
		log.Errorln("Mesos scheduler driver failed to start.")
		return stat, nil
	}

	log.Infof("Running Executor Driver with PID=%v\n", driver.self)
	return driver.Join()

}

// SendStatusUpdate sends status updates to the slave.
func (driver *MesosExecutorDriver) SendStatusUpdate(taskStatus *mesosproto.TaskStatus) (mesosproto.Status, error) {
	if driver.status != mesosproto.Status_DRIVER_RUNNING {
		return driver.status, nil
	}

	if taskStatus.GetState() == mesosproto.TaskState_TASK_STAGING {
		msg := "Executor is not allowed to send TASK_STAGING status update. Aborting!"
		err := fmt.Errorf(msg)
		log.Errorln(msg)
		driver.Abort()
		driver.exec.Error(driver, err.Error())
		return driver.status, err
	}

	// Set up status update.
	update := driver.makeStatusUpdate(taskStatus)
	log.Infof("Executor sending status update %v\n", update.String())

	// Capture the status update.
	driver.updates[uuid.UUID(update.GetUuid()).String()] = update

	// Put the status update in the message.
	message := &mesosproto.StatusUpdateMessage{
		Update: update,
		Pid:    proto.String(driver.self.String()),
	}
	// Send the message.
	if err := driver.messenger.Send(driver.slaveUPID, message); err != nil {
		msg := fmt.Sprintf("Failed to send %v: %v")
		err := fmt.Errorf(msg)
		log.Errorln(msg)
		driver.exec.Error(driver, msg)
		return driver.status, err
	}

	return driver.status, nil
}

func (driver *MesosExecutorDriver) makeStatusUpdate(taskStatus *mesosproto.TaskStatus) *mesosproto.StatusUpdate {
	now := float64(time.Now().Unix())
	// Fill in all the fields.
	taskStatus.Timestamp = proto.Float64(now)
	taskStatus.SlaveId = driver.slaveID
	update := &mesosproto.StatusUpdate{
		FrameworkId: driver.frameworkID,
		ExecutorId:  driver.executorID,
		SlaveId:     driver.slaveID,
		Status:      taskStatus,
		Timestamp:   proto.Float64(now),
		Uuid:        uuid.NewUUID(),
	}
	return update
}

// SendFrameworkMessage sends the framework message by sending a 'sendFrameworkMessageEvent'
// to the event loop, and receives the result from the response channel.
func (driver *MesosExecutorDriver) SendFrameworkMessage(data string) (mesosproto.Status, error) {
	log.V(3).Infoln("Sending framework message", string(data))

	if driver.status != mesosproto.Status_DRIVER_RUNNING {
		err := fmt.Errorf("Executor driver is not running")
		log.Errorln(err)
		return driver.status, err
	}

	message := &mesosproto.ExecutorToFrameworkMessage{
		SlaveId:     driver.slaveID,
		FrameworkId: driver.frameworkID,
		ExecutorId:  driver.executorID,
		Data:        []byte(data),
	}

	// Send the message.
	if err := driver.messenger.Send(driver.slaveUPID, message); err != nil {
		err := fmt.Errorf("Failed to send %v: %v")
		log.Errorln(err)
		return driver.status, err
	}
	return driver.status, nil
}
