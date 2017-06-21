package schedulerd

import (
	"crypto/md5"
	"encoding/binary"
	"strings"
	"sync"
	"time"

	"github.com/sensu/sensu-go/backend/messaging"
	"github.com/sensu/sensu-go/types"
)

//
// tldr;
//
// 1. Sleep until next next tick. (~1 second.)
// 2. If we've received a message on the 'stopping' channel go to step 8.
// 3. Grab the most recent copy of the state.
// 4. Iterate over the checks contained in our state. After exhausting list go to step 7.
// 5. For each check determine if duration of time until it's next execution is less than our delta. If so go to step 6 else continue iterating.
// 6. Spawn goroutine to build & publish check request. Go to step 4.
// 7. Go to step 1.
// 8. Done.
//

// SchedulerInterval frequency in which we schedule checks
const SchedulerInterval = 1 * time.Second

// Scheduler ...
type Scheduler struct {
	StateManager *StateManager
	MessageBus   messaging.MessageBus

	stopping  chan struct{}
	waitGroup *sync.WaitGroup
}

// Start ...
func (schedulerPtr *Scheduler) Start() {
	schedulerPtr.stopping = make(chan struct{})

	// Wait for runloop to close and tasks to finish
	wg := &sync.WaitGroup{}
	wg.Add(1)
	schedulerPtr.waitGroup = wg

	// Start interval
	interval := &SchedulerInterval{}
	interval.Start()

	go func() {
		defer wg.Done()

		for {
			interval.Wait()

			select {
			case <-schedulerPtr.stopping:
				return
			default:
				// Grab state atom
				state := schedulerPtr.StateManager.State()
				taskCtx := TaskContext{
					MessageBus: schedulerPtr.MessageBus,
					State:      &state,
				}

				// Iterate over every check looking for relevant tasks
				for _, check := range state.Checks() {
					task := Task{Check: check, Timeframe: &interval.Timeframe}

					// If the check should be run at this interval queue it
					if task.ShouldRun() {

						// TODO: As mentioned by grep, waitGroup is (probably?) extraneous.
						// If the message bus is no longer running it will return an error
						// but wont panic or cause any unintentional side-effects.
						DoTaskAsync(task, schedulerPtr.waitGroup)
					}
				}
			}
		}
	}()
}

// Stop ...
func (schedulerPtr *Scheduler) Stop() {
	close(schedulerPtr.stopping)
	schedulerPtr.waitGroup.Wait()
}

// TODO: Rename to TimeDelta or more simply Delta(?)
type Timeframe struct {
	Start time.Time
	// TODO: End/Delta time.Duration
	End time.Time
}

// TODO: Probably remove me.
func (framePtr *Timeframe) Contains(t *time.Time) bool {
	timeNano := t.UnixNano()
	return (timeNano >= framePtr.Start.UnixNano() && timeNano < framePtr.End.UnixNano())
}

type SchedulerInterval struct {
	Timeframe
}

func (intPtr *SchedulerInterval) Start() {
	time := time.Now()

	// Counter intuitively we also apply the start date to the End field. This is
	// because when the Wait method is first called it will copy of the contents
	// of End into the Start date.
	intPtr.Start = time
	intPtr.End = time
}

func (intPtr *SchedulerInterval) Wait() {
	// Apply the past end date to the our next start date. We use the end date
	// instead of the current time to ensure that there are never any gaps in our
	// time frame.
	intPtr.Start = intPtr.End

	// Set the end time to the nearest tick
	start := time.Now()
	delta := SchedulerInterval + (start.UnixNano() % SchedulerInterval) // TODO: Persist
	intPtr.End = start.Add(timeUntilNextInterval)                       // TODO: Remove & simply set delta

	// Sleep until were needed again
	time.Sleep(delta)
}

//
// TODO:
//
// Task, TaskContext & TaskRunner are far too generic. Run loop has a single
// resposiblity.
//
// Maybe pull over CheckExecutor from existing implentation?
//

// TaskContext ...
type TaskContext struct {
	State      *SchedulerState
	MessageBus messaging.MessageBus
}

// Task ...
type Task struct {
	Timeframe *Timeframe
	Check     *types.CheckConfig
}

// ShouldRun determine if the task should be run at this time
func (taskPtr *Task) ShouldRun() bool {
	interval := taskPtr.Check.Interval

	// Calculate a check execution splay to ensure
	// execution is consistent between process restarts.
	sum := md5.Sum([]byte(taskPtr.Check.Name))
	splay := binary.LittleEndian.Uint64(sum[:])

	// Determine the duration of time until our the next
	// time the task needs to be run
	t := uint64(taskPtr.Timeframe.Start.UnixNano())
	offset := (splay - t) % uint64(interval)
	timeUntilNextRun := time.Duration(offset) / time.Nanosecond

	// Return true if the next time the check should be run
	// intersects with the last timeframe.
	// TODO: Determine if timeUntilNextRun < Delta
	return taskPtr.Timeframe.Contains(t.Add(timeUntilNextRun))
}

// Run task in given context
func (taskPtr *Task) Run(ctx *TaskContext) error {
	run := TaskRunner{Task: &task, Ctx: &taskCtx}
	return run.Run()
}

// TaskRunner runs given task
type TaskRunner struct {
	Task *Task
	Ctx  *TaskContext
}

// Run ...
func (runPtr *TaskRunner) Run() error {
	// Build the check request we'll send off
	request := runPtr.buildRequest()

	// Publish check request to all subscribers
	err := runPtr.publishRequest(request)
	return err
}

// given check config fetches associated assets and builds request
func (runPtr *TaskRunner) buildRequest() *types.CheckRequest {
	check := runPtr.Task.Check

	// ...
	request := &types.CheckRequest{}
	request.Config = check

	// Guard against iterating over assets if there are no assets associated with
	// the check in the first place.
	if len(check.RuntimeAssets) == 0 {
		return request
	}

	// Explode assets; get assets & filter out those that are irrelevant
	allAssets := runPtr.Ctx.State.GetAssetsInOrg(check.Organization)
	for _, asset := range allAssets {
		if types.IsAssetIsRelevantToCheck(check, asset) {
			request.ExpandedAssets = append(request.ExpandedAssets, *asset)
		}
	}

	return request
}

func (runPtr *TaskRunner) publishRequest(request *types.CheckRequest) error {
	var err error

	check := runPtr.Check
	for _, sub := range check.Subscriptions {
		topic := messaging.SubscriptionTopic(check.Organization, sub)
		logger.Debugf("Sending check request for %s on topic %s", check.Name, topic)

		if pubErr := runPtr.Ctx.MessageBus.Publish(topic, request); err != nil {
			logger.Info("error publishing check request: ", err.Error())
			err = pubErr
		}
	}
	return err
}

// DoTaskAsync runs given task in goroutine and increments given waitgroup. Used
// so that we ensure that all tasks have been completed stopping the deamon.
func DoTaskAsync(task *Task, ctx *TaskContext, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		// TODO: What do we do w/ any errors? is there an error channel?
		task.Run(ctx)
		wg.Done()
	}()
}