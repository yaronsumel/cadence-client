// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package internal

// All code in this file is private to the package.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"go.uber.org/cadence/.gen/go/cadence/workflowserviceclient"
	s "go.uber.org/cadence/.gen/go/shared"
	"go.uber.org/cadence/encoded"
	"go.uber.org/cadence/internal/common"
	"go.uber.org/cadence/internal/common/backoff"
	"go.uber.org/cadence/internal/common/cache"
	"go.uber.org/cadence/internal/common/metrics"
	"go.uber.org/cadence/internal/common/util"
	"go.uber.org/zap"
)

const (
	defaultHeartBeatIntervalInSec = 10 * 60

	defaultStickyCacheSize = 10000
)

type (
	// workflowExecutionEventHandler process a single event.
	workflowExecutionEventHandler interface {
		// Process a single event and return the assosciated decisions.
		// Return List of decisions made, any error.
		ProcessEvent(event *s.HistoryEvent, isReplay bool, isLast bool) error
		// ProcessQuery process a query request.
		ProcessQuery(queryType string, queryArgs []byte) ([]byte, error)
		StackTrace() string
		// Close for cleaning up resources on this event handler
		Close()
	}

	// workflowTask wraps a decision task.
	workflowTask struct {
		task            *s.PollForDecisionTaskResponse
		historyIterator HistoryIterator
	}

	// activityTask wraps a activity task.
	activityTask struct {
		task          *s.PollForActivityTaskResponse
		pollStartTime time.Time
	}

	// resetStickinessTask wraps a ResetStickyTaskListRequest.
	resetStickinessTask struct {
		task *s.ResetStickyTaskListRequest
	}

	// workflowExecutionContextImpl is the cached workflow state for sticky execution
	workflowExecutionContextImpl struct {
		mutex             sync.Mutex
		workflowStartTime time.Time
		workflowInfo      *WorkflowInfo
		wth               *workflowTaskHandlerImpl

		eventHandler *workflowExecutionEventHandlerImpl

		isWorkflowCompleted bool
		result              []byte
		err                 error

		previousStartedEventID int64

		newDecisions        []*s.Decision
		currentDecisionTask *s.PollForDecisionTaskResponse
		laTunnel            *localActivityTunnel
		decisionStartTime   time.Time
	}

	// workflowTaskHandlerImpl is the implementation of WorkflowTaskHandler
	workflowTaskHandlerImpl struct {
		domain                         string
		metricsScope                   *metrics.TaggedScope
		ppMgr                          pressurePointMgr
		logger                         *zap.Logger
		identity                       string
		enableLoggingInReplay          bool
		disableStickyExecution         bool
		hostEnv                        *hostEnvImpl
		laTunnel                       *localActivityTunnel
		nonDeterministicWorkflowPolicy NonDeterministicWorkflowPolicy
		dataConverter                  encoded.DataConverter
	}

	activityProvider func(name string) activity
	// activityTaskHandlerImpl is the implementation of ActivityTaskHandler
	activityTaskHandlerImpl struct {
		taskListName     string
		identity         string
		service          workflowserviceclient.Interface
		metricsScope     *metrics.TaggedScope
		logger           *zap.Logger
		userContext      context.Context
		hostEnv          *hostEnvImpl
		activityProvider activityProvider
		dataConverter    encoded.DataConverter
	}

	// history wrapper method to help information about events.
	history struct {
		workflowTask  *workflowTask
		eventsHandler *workflowExecutionEventHandlerImpl
		loadedEvents  []*s.HistoryEvent
		currentIndex  int
		next          []*s.HistoryEvent
	}
)

func newHistory(task *workflowTask, eventsHandler *workflowExecutionEventHandlerImpl) *history {
	result := &history{
		workflowTask:  task,
		eventsHandler: eventsHandler,
		loadedEvents:  task.task.History.Events,
		currentIndex:  0,
	}

	return result
}

// Get workflow start event.
func (eh *history) GetWorkflowStartedEvent() (*s.HistoryEvent, error) {
	events := eh.workflowTask.task.History.Events
	if len(events) == 0 || events[0].GetEventType() != s.EventTypeWorkflowExecutionStarted {
		return nil, errors.New("unable to find WorkflowExecutionStartedEventAttributes in the history")
	}
	return events[0], nil
}

func (eh *history) IsReplayEvent(event *s.HistoryEvent) bool {
	return event.GetEventId() <= eh.workflowTask.task.GetPreviousStartedEventId() || isDecisionEvent(event.GetEventType())
}

func (eh *history) IsNextDecisionFailed() bool {
	events := eh.loadedEvents
	eventsSize := len(events)
	for i := eh.currentIndex; i < eventsSize; i++ {
		switch events[i].GetEventType() {
		case s.EventTypeDecisionTaskCompleted:
			return false
		case s.EventTypeDecisionTaskTimedOut:
			return true
		case s.EventTypeDecisionTaskFailed:
			return true
		}
	}
	return false
}

func isDecisionEvent(eventType s.EventType) bool {
	switch eventType {
	case s.EventTypeWorkflowExecutionCompleted,
		s.EventTypeWorkflowExecutionFailed,
		s.EventTypeWorkflowExecutionCanceled,
		s.EventTypeWorkflowExecutionContinuedAsNew,
		s.EventTypeActivityTaskScheduled,
		s.EventTypeActivityTaskCancelRequested, s.EventTypeRequestCancelActivityTaskFailed,
		s.EventTypeTimerStarted,
		s.EventTypeTimerCanceled, s.EventTypeCancelTimerFailed,
		s.EventTypeMarkerRecorded,
		s.EventTypeStartChildWorkflowExecutionInitiated,
		s.EventTypeRequestCancelExternalWorkflowExecutionInitiated,
		s.EventTypeSignalExternalWorkflowExecutionInitiated:
		return true
	default:
		return false
	}
}

// NextDecisionEvents returns events that there processed as new by the next decision.
// It also reorders events that were added to a history during outgoing decision. Without
// such reordering determinism is broken.
// For Ex: (pseudo code)
//   ResultA := Schedule_Activity_A
//   ResultB := Schedule_Activity_B
//   if ResultB.IsReady() { panic error }
//   ResultC := Schedule_Activity_C(ResultA)
// If both A and B activities complete then we could have two different paths, Either Scheduling C (or) Panic'ing.
// Workflow events:
// 	Workflow_Start, DecisionStart1, DecisionComplete1, A_Schedule, B_Schedule, A_Complete,
//      DecisionStart2, B_Complete, DecisionComplete2, C_Schedule.
// B_Complete happened concurrent to execution of the decision(2), where C_Schedule is a result made
// by execution of decision(2).
// To maintain determinism the concurrent decisions are moved to the one after the decisions made by current decision.
// markers result value returns marker events that currently running decision produced. They are used to
// implement SideEffect method execution without blocking on decision roundtrip.
func (eh *history) NextDecisionEvents() (result []*s.HistoryEvent, markers []*s.HistoryEvent, err error) {
	if eh.next == nil {
		eh.next, _, err = eh.nextDecisionEvents()
		if err != nil {
			return result, markers, err
		}
	}

	result = eh.next
	if len(result) > 0 {
		eh.next, markers, err = eh.nextDecisionEvents()
	}
	return result, markers, err
}

func (eh *history) hasMoreEvents() bool {
	historyIterator := eh.workflowTask.historyIterator
	return historyIterator != nil && historyIterator.HasNextPage()
}

func (eh *history) getMoreEvents() (*s.History, error) {
	return eh.workflowTask.historyIterator.GetNextPage()
}

func (eh *history) nextDecisionEvents() (nextEvents []*s.HistoryEvent, markers []*s.HistoryEvent, err error) {
	if eh.currentIndex == len(eh.loadedEvents) && !eh.hasMoreEvents() {
		return []*s.HistoryEvent{}, []*s.HistoryEvent{}, nil
	}

	// Process events

OrderEvents:
	for {
		// load more history events if needed
		for eh.currentIndex == len(eh.loadedEvents) {
			if !eh.hasMoreEvents() {
				break OrderEvents
			}
			historyPage, err1 := eh.getMoreEvents()
			if err1 != nil {
				err = err1
				return
			}
			eh.loadedEvents = append(eh.loadedEvents, historyPage.Events...)
		}

		event := eh.loadedEvents[eh.currentIndex]
		switch event.GetEventType() {
		case s.EventTypeDecisionTaskStarted:
			if !eh.IsNextDecisionFailed() {
				eh.currentIndex++
				nextEvents = append(nextEvents, event)
				break OrderEvents
			}

		case s.EventTypeDecisionTaskCompleted,
			s.EventTypeDecisionTaskScheduled,
			s.EventTypeDecisionTaskTimedOut,
			s.EventTypeDecisionTaskFailed:
			// Skip
		default:
			if isPreloadMarkerEvent(event) {
				markers = append(markers, event)
			}
			nextEvents = append(nextEvents, event)
		}
		eh.currentIndex++
	}

	// shrink loaded events so it can be GCed
	eh.loadedEvents = eh.loadedEvents[eh.currentIndex:]
	eh.currentIndex = 0

	return nextEvents, markers, nil
}

func isPreloadMarkerEvent(event *s.HistoryEvent) bool {
	return event.GetEventType() == s.EventTypeMarkerRecorded
}

// newWorkflowTaskHandler returns an implementation of workflow task handler.
func newWorkflowTaskHandler(
	domain string,
	params workerExecutionParameters,
	ppMgr pressurePointMgr,
	hostEnv *hostEnvImpl,
) WorkflowTaskHandler {
	ensureRequiredParams(&params)
	return &workflowTaskHandlerImpl{
		domain:                 domain,
		logger:                 params.Logger,
		ppMgr:                  ppMgr,
		metricsScope:           metrics.NewTaggedScope(params.MetricsScope),
		identity:               params.Identity,
		enableLoggingInReplay:  params.EnableLoggingInReplay,
		disableStickyExecution: params.DisableStickyExecution,
		hostEnv:                hostEnv,
		nonDeterministicWorkflowPolicy: params.NonDeterministicWorkflowPolicy,
		dataConverter:                  params.DataConverter,
	}
}

// TODO: need a better eviction policy based on memory usage
var workflowCache cache.Cache
var stickyCacheSize = defaultStickyCacheSize
var initCacheOnce sync.Once
var stickyCacheLock sync.Mutex

// SetStickyWorkflowCacheSize sets the cache size for sticky workflow cache. Sticky workflow execution is the affinity
// between decision tasks of a specific workflow execution to a specific worker. The affinity is set if sticky execution
// is enabled via Worker.Options (It is enabled by default unless disabled explicitly). The benefit of sticky execution
// is that workflow does not have to reconstruct the state by replaying from beginning of history events. But the cost
// is it consumes more memory as it rely on caching workflow execution's running state on the worker. The cache is shared
// between workers running within same process. This must be called before any worker is started. If not called, the
// default size of 10K (might change in future) will be used.
func SetStickyWorkflowCacheSize(cacheSize int) {
	stickyCacheLock.Lock()
	defer stickyCacheLock.Unlock()
	if workflowCache != nil {
		panic("cache already created, please set cache size before worker starts.")
	}
	stickyCacheSize = cacheSize
}

func getWorkflowCache() cache.Cache {
	initCacheOnce.Do(func() {
		stickyCacheLock.Lock()
		defer stickyCacheLock.Unlock()
		workflowCache = cache.New(stickyCacheSize, &cache.Options{
			RemovedFunc: func(cachedEntity interface{}) {
				wc := cachedEntity.(*workflowExecutionContextImpl)
				wc.onEviction()
			},
		})
	})
	return workflowCache
}

func getWorkflowContext(runID string) *workflowExecutionContextImpl {
	o := getWorkflowCache().Get(runID)
	if o == nil {
		return nil
	}
	wc := o.(*workflowExecutionContextImpl)
	return wc
}

func putWorkflowContext(runID string, wc *workflowExecutionContextImpl) (*workflowExecutionContextImpl, error) {
	existing, err := getWorkflowCache().PutIfNotExist(runID, wc)
	if err != nil {
		return nil, err
	}
	return existing.(*workflowExecutionContextImpl), nil
}

func removeWorkflowContext(runID string) {
	getWorkflowCache().Delete(runID)
}

func (w *workflowExecutionContextImpl) Lock() {
	w.mutex.Lock()
}

func (w *workflowExecutionContextImpl) Unlock(err error) {
	if err != nil || w.err != nil || w.isWorkflowCompleted || (w.wth.disableStickyExecution && !w.hasPendingLocalActivityWork()) {
		// TODO: in case of closed, it asumes the close decision always succeed. need server side change to return
		// error to indicate the close failure case. This should be rear case. For now, always remove the cache, and
		// if the close decision failed, the next decision will have to rebuild the state.
		removeWorkflowContext(w.workflowInfo.WorkflowExecution.RunID)
	}

	w.mutex.Unlock()
}

func (w *workflowExecutionContextImpl) completeWorkflow(result []byte, err error) {
	w.isWorkflowCompleted = true
	w.result = result
	w.err = err
}

func (w *workflowExecutionContextImpl) shouldResetStickyOnEviction() bool {
	// Not all evictions from the cache warrant a call to the server
	// to reset stickiness.
	// Cases when this is redundant or unnecessary include
	// when an error was encountered during execution
	// or workflow simply completed successfully.
	return w.err == nil && !w.isWorkflowCompleted
}

func (w *workflowExecutionContextImpl) onEviction() {
	// onEviction is run by LRU cache's removeFunc in separate goroutinue
	w.mutex.Lock()

	// Queue a ResetStickiness request *BEFORE* calling clearState
	// because once destroyed, no sensible information
	// may be ascertained about the execution context's state,
	// nor should any of its methods be invoked.
	if w.shouldResetStickyOnEviction() {
		w.queueResetStickinessTask()
	}

	w.clearState()
	w.mutex.Unlock()
}

func (w *workflowExecutionContextImpl) isDestroyed() bool {
	return w.eventHandler == nil
}

func (w *workflowExecutionContextImpl) queueResetStickinessTask() {
	var task resetStickinessTask
	task.task = &s.ResetStickyTaskListRequest{
		Domain: common.StringPtr(w.workflowInfo.Domain),
		Execution: &s.WorkflowExecution{
			WorkflowId: common.StringPtr(w.workflowInfo.WorkflowExecution.ID),
			RunId:      common.StringPtr(w.workflowInfo.WorkflowExecution.RunID),
		},
	}
	w.laTunnel.resultCh <- &task
}

func (w *workflowExecutionContextImpl) clearState() {
	w.clearCurrentTask()
	w.isWorkflowCompleted = false
	w.result = nil
	w.err = nil
	w.previousStartedEventID = 0
	w.newDecisions = nil
	if w.eventHandler != nil {
		w.eventHandler.Close()
		w.eventHandler = nil
	}
}

func (w *workflowExecutionContextImpl) createEventHandler() {
	w.clearState()
	w.eventHandler = newWorkflowExecutionEventHandler(
		w.workflowInfo,
		w.completeWorkflow,
		w.wth.logger,
		w.wth.enableLoggingInReplay,
		w.wth.metricsScope,
		w.wth.hostEnv,
		w.wth.dataConverter).(*workflowExecutionEventHandlerImpl)
}

func resetHistory(task *s.PollForDecisionTaskResponse, historyIterator HistoryIterator) (*s.History, error) {
	historyIterator.Reset()
	firstPageHistory, err := historyIterator.GetNextPage()
	if err != nil {
		return nil, err
	}
	task.History = firstPageHistory
	return firstPageHistory, nil
}

func (wth *workflowTaskHandlerImpl) createWorkflowContext(task *s.PollForDecisionTaskResponse) (*workflowExecutionContextImpl, error) {
	h := task.History
	attributes := h.Events[0].WorkflowExecutionStartedEventAttributes
	if attributes == nil {
		return nil, errors.New("first history event is not WorkflowExecutionStarted")
	}
	taskList := attributes.TaskList
	if taskList == nil {
		return nil, errors.New("nil TaskList in WorkflowExecutionStarted event")
	}

	runID := task.WorkflowExecution.GetRunId()
	workflowID := task.WorkflowExecution.GetWorkflowId()

	// Setup workflow Info
	workflowInfo := &WorkflowInfo{
		WorkflowType: flowWorkflowTypeFrom(*task.WorkflowType),
		TaskListName: taskList.GetName(),
		WorkflowExecution: WorkflowExecution{
			ID:    workflowID,
			RunID: runID,
		},
		ExecutionStartToCloseTimeoutSeconds: attributes.GetExecutionStartToCloseTimeoutSeconds(),
		TaskStartToCloseTimeoutSeconds:      attributes.GetTaskStartToCloseTimeoutSeconds(),
		Domain:  wth.domain,
		Attempt: attributes.GetAttempt(),
	}

	wfStartTime := time.Unix(0, h.Events[0].GetTimestamp())
	workflowContext := &workflowExecutionContextImpl{workflowStartTime: wfStartTime, workflowInfo: workflowInfo, wth: wth}
	workflowContext.createEventHandler()

	return workflowContext, nil
}

func (wth *workflowTaskHandlerImpl) getOrCreateWorkflowContext(task *s.PollForDecisionTaskResponse,
	historyIterator HistoryIterator) (workflowContext *workflowExecutionContextImpl, err error) {
	metricsScope := wth.metricsScope.GetTaggedScope(tagWorkflowType, task.WorkflowType.GetName())
	defer func() {
		if err == nil && workflowContext != nil {
			workflowContext.laTunnel = wth.laTunnel
		}
		metricsScope.Gauge(metrics.StickyCacheSize).Update(float64(workflowCache.Size()))
	}()

	runID := task.WorkflowExecution.GetRunId()

	history := task.History
	isFullHistory := isFullHistory(history)

	workflowContext = nil
	if task.Query == nil || (task.Query != nil && !isFullHistory) {
		workflowContext = getWorkflowContext(runID)
	}

	if workflowContext != nil {
		workflowContext.Lock()
		if task.Query != nil && !isFullHistory {
			// query task and we have a valid cached state
			metricsScope.Counter(metrics.StickyCacheHit).Inc(1)
		} else if history.Events[0].GetEventId() == workflowContext.previousStartedEventID+1 {
			// non query task and we have a valid cached state
			metricsScope.Counter(metrics.StickyCacheHit).Inc(1)
		} else {
			// non query task and cached state is missing events, we need to discard the cached state and rebuild one.
			workflowContext.ResetIfStale(task, historyIterator)
		}
	} else {
		if !isFullHistory {
			// we are getting partial history task, but cached state was already evicted.
			// we need to reset history so we get events from beginning to replay/rebuild the state
			metricsScope.Counter(metrics.StickyCacheMiss).Inc(1)
			if history, err = resetHistory(task, historyIterator); err != nil {
				return
			}
		}

		if workflowContext, err = wth.createWorkflowContext(task); err != nil {
			return
		}

		if !wth.disableStickyExecution && task.Query == nil {
			workflowContext, _ = putWorkflowContext(runID, workflowContext)
		}
		workflowContext.Lock()
	}

	err = workflowContext.resetStateIfDestroyed(task, historyIterator)

	return
}

func isFullHistory(history *s.History) bool {
	if len(history.Events) == 0 || history.Events[0].GetEventType() != s.EventTypeWorkflowExecutionStarted {
		return false
	}
	return true
}

func (w *workflowExecutionContextImpl) resetStateIfDestroyed(task *s.PollForDecisionTaskResponse, historyIterator HistoryIterator) error {
	// It is possible that 2 threads (one for decision task and one for query task) that both are getting this same
	// cached workflowContext. If one task finished with err, it would destroy the cached state. In that case, the
	// second task needs to reset the cache state and start from beginning of the history.
	if w.isDestroyed() {
		w.createEventHandler()
		// reset history events if necessary
		if !isFullHistory(task.History) {
			if _, err := resetHistory(task, historyIterator); err != nil {
				return err
			}
		}
	}
	return nil
}

// ProcessWorkflowTask processes all the events of the workflow task.
func (wth *workflowTaskHandlerImpl) ProcessWorkflowTask(
	task *s.PollForDecisionTaskResponse,
	historyIterator HistoryIterator,
) (completeRequest interface{}, context WorkflowExecutionContext, err error) {
	if task == nil {
		return nil, nil, errors.New("nil workflow task provided")
	}

	if task.History == nil || len(task.History.Events) == 0 {
		task.History = &s.History{
			Events: []*s.HistoryEvent{},
		}
	}
	if task.Query == nil && len(task.History.Events) == 0 {
		return nil, nil, errors.New("nil or empty history")
	}

	runID := task.WorkflowExecution.GetRunId()
	workflowID := task.WorkflowExecution.GetWorkflowId()
	traceLog(func() {
		wth.logger.Debug("Processing new workflow task.",
			zap.String(tagWorkflowType, task.WorkflowType.GetName()),
			zap.String(tagWorkflowID, workflowID),
			zap.String(tagRunID, runID),
			zap.Int64("PreviousStartedEventId", task.GetPreviousStartedEventId()))
	})

	workflowContext, err := wth.getOrCreateWorkflowContext(task, historyIterator)
	if err != nil {
		return nil, nil, err
	}

	defer func() {
		workflowContext.Unlock(err)
	}()

	response, err := workflowContext.ProcessWorkflowTask(task, historyIterator)
	return response, workflowContext, err
}

func (w *workflowExecutionContextImpl) ProcessWorkflowTask(task *s.PollForDecisionTaskResponse, historyIterator HistoryIterator) (completeRequest interface{}, err error) {
	if err = w.ResetIfStale(task, historyIterator); err != nil {
		return
	}
	w.SetCurrentTask(task)

	eventHandler := w.eventHandler
	reorderedHistory := newHistory(&workflowTask{task, historyIterator}, eventHandler)
	var replayDecisions []*s.Decision
	var respondEvents []*s.HistoryEvent

	skipReplayCheck := w.skipReplayCheck()
	// Process events
ProcessEvents:
	for {
		reorderedEvents, markers, err := reorderedHistory.NextDecisionEvents()
		if err != nil {
			return nil, err
		}

		if len(reorderedEvents) == 0 {
			break ProcessEvents
		}
		// Markers are from the events that are produced from the current decision
		for _, m := range markers {
			if m.MarkerRecordedEventAttributes.GetMarkerName() != localActivityMarkerName {
				// local activity marker needs to be applied after decision task started event
				err := eventHandler.ProcessEvent(m, true, false)
				if err != nil {
					return nil, err
				}
			}
		}

		for i, event := range reorderedEvents {
			isInReplay := reorderedHistory.IsReplayEvent(event)
			isLast := !isInReplay && i == len(reorderedEvents)-1
			if !skipReplayCheck && isDecisionEvent(event.GetEventType()) {
				respondEvents = append(respondEvents, event)
			}

			if isPreloadMarkerEvent(event) {
				// marker events are processed separately
				continue
			}

			// Any pressure points.
			err := w.wth.executeAnyPressurePoints(event, isInReplay)
			if err != nil {
				return nil, err
			}

			err = eventHandler.ProcessEvent(event, isInReplay, isLast)
			if err != nil {
				return nil, err
			}
		}

		// now apply local activity markers
		for _, m := range markers {
			if m.MarkerRecordedEventAttributes.GetMarkerName() == localActivityMarkerName {
				err := eventHandler.ProcessEvent(m, true, false)
				if err != nil {
					return nil, err
				}
			}
		}
		isReplay := len(reorderedEvents) > 0 && reorderedHistory.IsReplayEvent(reorderedEvents[len(reorderedEvents)-1])
		if isReplay {
			eventDecisions := eventHandler.decisionsHelper.getDecisions(true)
			if len(eventDecisions) > 0 && !skipReplayCheck {
				replayDecisions = append(replayDecisions, eventDecisions...)
			}
		}
	}

	if !skipReplayCheck {
		// check if decisions from reply matches to the history events
		if err := matchReplayWithHistory(replayDecisions, respondEvents); err != nil {
			w.wth.metricsScope.GetTaggedScope(tagWorkflowType, task.WorkflowType.GetName()).Counter(metrics.NonDeterministicError).Inc(1)
			w.wth.logger.Error("Replay and history mismatch.",
				zap.String(tagWorkflowType, task.WorkflowType.GetName()),
				zap.String(tagWorkflowID, task.WorkflowExecution.GetWorkflowId()),
				zap.String(tagRunID, task.WorkflowExecution.GetRunId()),
				zap.Error(err))

			// Whether or not we store the error in workflowContext.err makes
			// a significant difference, to the point that it affects client's observable
			// behavior as far as handling non-deterministic workflows.
			//
			// If we store it in workflowContext.err, the decision task completion code
			// will pick up the error and correctly wrap it in the response request we sent back
			// to the server, which in this case will contain a request to fail the workflow.
			//
			// If we simply return the error, the decision task completion code path will not
			// execute at all, therefore, no response is sent back to the server and we will
			// look like a decision task time out.
			switch w.wth.nonDeterministicWorkflowPolicy {
			case NonDeterministicWorkflowPolicyFailWorkflow:
				eventHandler.Complete(nil, NewCustomError("nondeterministic workflow", err.Error()))
			case NonDeterministicWorkflowPolicyBlockWorkflow:
				return nil, err
			default:
				panic(fmt.Sprintf("unknown mismatched workflow history policy."))
			}
		}
	}

	return w.CompleteDecisionTask(true), nil
}

func (w *workflowExecutionContextImpl) ProcessLocalActivityResult(lar *localActivityResult) (interface{}, error) {
	err := w.eventHandler.ProcessLocalActivityResult(lar)
	if err != nil {
		return nil, err
	}

	return w.CompleteDecisionTask(true), nil
}

func (w *workflowExecutionContextImpl) CompleteDecisionTask(waitLocalActivities bool) interface{} {
	if w.currentDecisionTask == nil {
		return nil
	}
	// w.laTunnel could be nil for worker.ReplayHistory() because there is no worker started, in that case we don't
	// care about the pending local activities, and just return because the result is ignored anyway by the caller.
	if w.hasPendingLocalActivityWork() && w.laTunnel != nil {
		if len(w.eventHandler.unstartedLaTasks) > 0 {
			// start new local activity tasks
			for activityID := range w.eventHandler.unstartedLaTasks {
				task := w.eventHandler.pendingLaTasks[activityID]
				task.wc = w
				w.laTunnel.sendTask(task)
			}
			w.eventHandler.unstartedLaTasks = make(map[string]struct{})
		}
		// cannot complete decision task as there are pending local activities
		if waitLocalActivities {
			return nil
		}
	}

	eventDecisions := w.eventHandler.decisionsHelper.getDecisions(true)
	if len(eventDecisions) > 0 {
		w.newDecisions = append(w.newDecisions, eventDecisions...)
	}

	completeRequest := w.wth.completeWorkflow(w.eventHandler, w.currentDecisionTask, w, w.newDecisions, !waitLocalActivities)
	w.clearCurrentTask()

	return completeRequest
}

func (w *workflowExecutionContextImpl) hasPendingLocalActivityWork() bool {
	return !w.isWorkflowCompleted && w.eventHandler != nil && len(w.eventHandler.pendingLaTasks) > 0
}

func (w *workflowExecutionContextImpl) clearCurrentTask() {
	w.newDecisions = nil
	w.currentDecisionTask = nil
}

func (w *workflowExecutionContextImpl) skipReplayCheck() bool {
	return w.currentDecisionTask.Query != nil || !isFullHistory(w.currentDecisionTask.History)
}

func (w *workflowExecutionContextImpl) GetCurrentDecisionTask() *s.PollForDecisionTaskResponse {
	return w.currentDecisionTask
}

func (w *workflowExecutionContextImpl) SetCurrentTask(task *s.PollForDecisionTaskResponse) {
	w.currentDecisionTask = task
	// do not update the previousStartedEventID for query task
	if task.Query == nil {
		w.previousStartedEventID = task.GetStartedEventId()
	}
	w.decisionStartTime = time.Now()
}

func (w *workflowExecutionContextImpl) ResetIfStale(task *s.PollForDecisionTaskResponse, historyIterator HistoryIterator) error {
	if len(task.History.Events) > 0 && task.History.Events[0].GetEventId() != w.previousStartedEventID+1 {
		w.wth.logger.Debug("Cached state staled, new task has unexpected events",
			zap.String(tagWorkflowID, task.WorkflowExecution.GetWorkflowId()),
			zap.String(tagRunID, task.WorkflowExecution.GetRunId()),
			zap.Int64("CachedPreviousStartedEventID", w.previousStartedEventID),
			zap.Int64("TaskFirstEventID", task.History.Events[0].GetEventId()),
			zap.Int64("TaskStartedEventID", task.GetStartedEventId()),
			zap.Int64("TaskPreviousStartedEventID", task.GetPreviousStartedEventId()))

		w.wth.metricsScope.
			GetTaggedScope(tagWorkflowType, task.WorkflowType.GetName()).
			Counter(metrics.StickyCacheStall).Inc(1)

		w.clearState()
		return w.resetStateIfDestroyed(task, historyIterator)
	}
	return nil
}

func (w *workflowExecutionContextImpl) GetDecisionTimeout() time.Duration {
	return time.Second * time.Duration(w.workflowInfo.TaskStartToCloseTimeoutSeconds)
}

func (w *workflowExecutionContextImpl) StackTrace() string {
	if w.eventHandler == nil {
		return "eventHandler is closed"
	}
	return w.eventHandler.StackTrace()
}

func skipDeterministicCheckForDecision(d *s.Decision) bool {
	if d.GetDecisionType() == s.DecisionTypeRecordMarker {
		markerName := d.RecordMarkerDecisionAttributes.GetMarkerName()
		if markerName == versionMarkerName || markerName == mutableSideEffectMarkerName {
			return true
		}
	}
	return false
}

func skipDeterministicCheckForEvent(e *s.HistoryEvent) bool {
	if e.GetEventType() == s.EventTypeMarkerRecorded {
		markerName := e.MarkerRecordedEventAttributes.GetMarkerName()
		if markerName == versionMarkerName || markerName == mutableSideEffectMarkerName {
			return true
		}
	}
	return false
}

func matchReplayWithHistory(replayDecisions []*s.Decision, historyEvents []*s.HistoryEvent) error {
	di := 0
	hi := 0
	hSize := len(historyEvents)
	dSize := len(replayDecisions)
matchLoop:
	for hi < hSize || di < dSize {
		var e *s.HistoryEvent
		if hi < hSize {
			e = historyEvents[hi]
			if skipDeterministicCheckForEvent(e) {
				hi++
				continue matchLoop
			}
		}

		var d *s.Decision
		if di < dSize {
			d = replayDecisions[di]
			if skipDeterministicCheckForDecision(d) {
				di++
				continue matchLoop
			}
		}

		if d == nil {
			return fmt.Errorf("nondeterministic workflow: missing replay decision for %s", util.HistoryEventToString(e))
		}

		if e == nil {
			return fmt.Errorf("nondeterministic workflow: extra replay decision for %s", util.DecisionToString(d))
		}

		if !isDecisionMatchEvent(d, e, false) {
			return fmt.Errorf("nondeterministic workflow: history event is %s, replay decision is %s",
				util.HistoryEventToString(e), util.DecisionToString(d))
		}

		di++
		hi++
	}
	return nil
}

func lastPartOfName(name string) string {
	lastDotIdx := strings.LastIndex(name, ".")
	if lastDotIdx < 0 || lastDotIdx == len(name)-1 {
		return name
	}
	return name[lastDotIdx+1:]
}

func isDecisionMatchEvent(d *s.Decision, e *s.HistoryEvent, strictMode bool) bool {
	switch d.GetDecisionType() {
	case s.DecisionTypeScheduleActivityTask:
		if e.GetEventType() != s.EventTypeActivityTaskScheduled {
			return false
		}
		eventAttributes := e.ActivityTaskScheduledEventAttributes
		decisionAttributes := d.ScheduleActivityTaskDecisionAttributes

		if eventAttributes.GetActivityId() != decisionAttributes.GetActivityId() ||
			lastPartOfName(eventAttributes.ActivityType.GetName()) != lastPartOfName(decisionAttributes.ActivityType.GetName()) ||
			(strictMode && eventAttributes.TaskList.GetName() != decisionAttributes.TaskList.GetName()) ||
			(strictMode && bytes.Compare(eventAttributes.Input, decisionAttributes.Input) != 0) {
			return false
		}

		return true

	case s.DecisionTypeRequestCancelActivityTask:
		if e.GetEventType() != s.EventTypeActivityTaskCancelRequested && e.GetEventType() != s.EventTypeRequestCancelActivityTaskFailed {
			return false
		}
		decisionAttributes := d.RequestCancelActivityTaskDecisionAttributes
		if e.GetEventType() == s.EventTypeActivityTaskCancelRequested {
			eventAttributes := e.ActivityTaskCancelRequestedEventAttributes
			if eventAttributes.GetActivityId() != decisionAttributes.GetActivityId() {
				return false
			}
		} else if e.GetEventType() == s.EventTypeRequestCancelActivityTaskFailed {
			eventAttributes := e.RequestCancelActivityTaskFailedEventAttributes
			if eventAttributes.GetActivityId() != decisionAttributes.GetActivityId() {
				return false
			}
		}

		return true

	case s.DecisionTypeStartTimer:
		if e.GetEventType() != s.EventTypeTimerStarted {
			return false
		}
		eventAttributes := e.TimerStartedEventAttributes
		decisionAttributes := d.StartTimerDecisionAttributes

		if eventAttributes.GetTimerId() != decisionAttributes.GetTimerId() ||
			(strictMode && eventAttributes.GetStartToFireTimeoutSeconds() != decisionAttributes.GetStartToFireTimeoutSeconds()) {
			return false
		}

		return true

	case s.DecisionTypeCancelTimer:
		if e.GetEventType() != s.EventTypeTimerCanceled && e.GetEventType() != s.EventTypeCancelTimerFailed {
			return false
		}
		decisionAttributes := d.CancelTimerDecisionAttributes
		if e.GetEventType() == s.EventTypeTimerCanceled {
			eventAttributes := e.TimerCanceledEventAttributes
			if eventAttributes.GetTimerId() != decisionAttributes.GetTimerId() {
				return false
			}
		} else if e.GetEventType() == s.EventTypeCancelTimerFailed {
			eventAttributes := e.CancelTimerFailedEventAttributes
			if eventAttributes.GetTimerId() != decisionAttributes.GetTimerId() {
				return false
			}
		}

		return true

	case s.DecisionTypeCompleteWorkflowExecution:
		if e.GetEventType() != s.EventTypeWorkflowExecutionCompleted {
			return false
		}
		if strictMode {
			eventAttributes := e.WorkflowExecutionCompletedEventAttributes
			decisionAttributes := d.CompleteWorkflowExecutionDecisionAttributes

			if bytes.Compare(eventAttributes.Result, decisionAttributes.Result) != 0 {
				return false
			}
		}

		return true

	case s.DecisionTypeFailWorkflowExecution:
		if e.GetEventType() != s.EventTypeWorkflowExecutionFailed {
			return false
		}
		if strictMode {
			eventAttributes := e.WorkflowExecutionFailedEventAttributes
			decisionAttributes := d.FailWorkflowExecutionDecisionAttributes

			if eventAttributes.GetReason() != decisionAttributes.GetReason() ||
				bytes.Compare(eventAttributes.Details, decisionAttributes.Details) != 0 {
				return false
			}
		}

		return true

	case s.DecisionTypeRecordMarker:
		if e.GetEventType() != s.EventTypeMarkerRecorded {
			return false
		}
		eventAttributes := e.MarkerRecordedEventAttributes
		decisionAttributes := d.RecordMarkerDecisionAttributes
		if eventAttributes.GetMarkerName() != decisionAttributes.GetMarkerName() {
			return false
		}

		return true

	case s.DecisionTypeRequestCancelExternalWorkflowExecution:
		if e.GetEventType() != s.EventTypeRequestCancelExternalWorkflowExecutionInitiated {
			return false
		}
		eventAttributes := e.RequestCancelExternalWorkflowExecutionInitiatedEventAttributes
		decisionAttributes := d.RequestCancelExternalWorkflowExecutionDecisionAttributes
		if eventAttributes.GetDomain() != decisionAttributes.GetDomain() ||
			eventAttributes.WorkflowExecution.GetWorkflowId() != decisionAttributes.GetWorkflowId() {
			return false
		}

		return true

	case s.DecisionTypeSignalExternalWorkflowExecution:
		if e.GetEventType() != s.EventTypeSignalExternalWorkflowExecutionInitiated {
			return false
		}
		eventAttributes := e.SignalExternalWorkflowExecutionInitiatedEventAttributes
		decisionAttributes := d.SignalExternalWorkflowExecutionDecisionAttributes
		if eventAttributes.GetDomain() != decisionAttributes.GetDomain() ||
			eventAttributes.GetSignalName() != decisionAttributes.GetSignalName() ||
			eventAttributes.WorkflowExecution.GetWorkflowId() != decisionAttributes.Execution.GetWorkflowId() {
			return false
		}

		return true

	case s.DecisionTypeCancelWorkflowExecution:
		if e.GetEventType() != s.EventTypeWorkflowExecutionCanceled {
			return false
		}
		if strictMode {
			eventAttributes := e.WorkflowExecutionCanceledEventAttributes
			decisionAttributes := d.CancelWorkflowExecutionDecisionAttributes
			if bytes.Compare(eventAttributes.Details, decisionAttributes.Details) != 0 {
				return false
			}
		}
		return true

	case s.DecisionTypeContinueAsNewWorkflowExecution:
		if e.GetEventType() != s.EventTypeWorkflowExecutionContinuedAsNew {
			return false
		}

		return true

	case s.DecisionTypeStartChildWorkflowExecution:
		if e.GetEventType() != s.EventTypeStartChildWorkflowExecutionInitiated {
			return false
		}
		eventAttributes := e.StartChildWorkflowExecutionInitiatedEventAttributes
		decisionAttributes := d.StartChildWorkflowExecutionDecisionAttributes
		if lastPartOfName(eventAttributes.WorkflowType.GetName()) != lastPartOfName(decisionAttributes.WorkflowType.GetName()) ||
			(strictMode && eventAttributes.GetDomain() != decisionAttributes.GetDomain()) ||
			(strictMode && eventAttributes.TaskList.GetName() != decisionAttributes.TaskList.GetName()) {
			return false
		}

		return true
	}

	return false
}

func (wth *workflowTaskHandlerImpl) completeWorkflow(
	eventHandler *workflowExecutionEventHandlerImpl,
	task *s.PollForDecisionTaskResponse,
	workflowContext *workflowExecutionContextImpl,
	decisions []*s.Decision,
	forceNewDecision bool) interface{} {

	// for query task
	if task.Query != nil {
		queryCompletedRequest := &s.RespondQueryTaskCompletedRequest{TaskToken: task.TaskToken}
		if panicErr, ok := workflowContext.err.(*PanicError); ok {
			queryCompletedRequest.CompletedType = common.QueryTaskCompletedTypePtr(s.QueryTaskCompletedTypeFailed)
			queryCompletedRequest.ErrorMessage = common.StringPtr("Workflow panic: " + panicErr.Error())
			return queryCompletedRequest
		}

		result, err := eventHandler.ProcessQuery(task.Query.GetQueryType(), task.Query.QueryArgs)
		if err != nil {
			queryCompletedRequest.CompletedType = common.QueryTaskCompletedTypePtr(s.QueryTaskCompletedTypeFailed)
			queryCompletedRequest.ErrorMessage = common.StringPtr(err.Error())
		} else {
			queryCompletedRequest.CompletedType = common.QueryTaskCompletedTypePtr(s.QueryTaskCompletedTypeCompleted)
			queryCompletedRequest.QueryResult = result
		}
		return queryCompletedRequest
	}

	metricsScope := wth.metricsScope.GetTaggedScope(tagWorkflowType, eventHandler.workflowEnvironmentImpl.workflowInfo.WorkflowType.Name)

	// fail decision task on decider panic
	if panicErr, ok := workflowContext.err.(*PanicError); ok {
		// Workflow panic
		metricsScope.Counter(metrics.DecisionTaskPanicCounter).Inc(1)
		wth.logger.Error("Workflow panic.",
			zap.String("PanicError", panicErr.Error()),
			zap.String("PanicStack", panicErr.StackTrace()))
		return errorToFailDecisionTask(task.TaskToken, panicErr, wth.identity)
	}

	// complete decision task
	var closeDecision *s.Decision
	if canceledErr, ok := workflowContext.err.(*CanceledError); ok {
		// Workflow cancelled
		metricsScope.Counter(metrics.WorkflowCanceledCounter).Inc(1)
		closeDecision = createNewDecision(s.DecisionTypeCancelWorkflowExecution)
		_, details := getErrorDetails(canceledErr, wth.dataConverter)
		closeDecision.CancelWorkflowExecutionDecisionAttributes = &s.CancelWorkflowExecutionDecisionAttributes{
			Details: details,
		}
	} else if contErr, ok := workflowContext.err.(*ContinueAsNewError); ok {
		// Continue as new error.
		metricsScope.Counter(metrics.WorkflowContinueAsNewCounter).Inc(1)
		closeDecision = createNewDecision(s.DecisionTypeContinueAsNewWorkflowExecution)
		closeDecision.ContinueAsNewWorkflowExecutionDecisionAttributes = &s.ContinueAsNewWorkflowExecutionDecisionAttributes{
			WorkflowType: workflowTypePtr(*contErr.params.workflowType),
			Input:        contErr.params.input,
			TaskList:     common.TaskListPtr(s.TaskList{Name: contErr.params.taskListName}),
			ExecutionStartToCloseTimeoutSeconds: contErr.params.executionStartToCloseTimeoutSeconds,
			TaskStartToCloseTimeoutSeconds:      contErr.params.taskStartToCloseTimeoutSeconds,
		}
	} else if workflowContext.err != nil {
		// Workflow failures
		metricsScope.Counter(metrics.WorkflowFailedCounter).Inc(1)
		closeDecision = createNewDecision(s.DecisionTypeFailWorkflowExecution)
		reason, details := getErrorDetails(workflowContext.err, wth.dataConverter)
		closeDecision.FailWorkflowExecutionDecisionAttributes = &s.FailWorkflowExecutionDecisionAttributes{
			Reason:  common.StringPtr(reason),
			Details: details,
		}
	} else if workflowContext.isWorkflowCompleted {
		// Workflow completion
		metricsScope.Counter(metrics.WorkflowCompletedCounter).Inc(1)
		closeDecision = createNewDecision(s.DecisionTypeCompleteWorkflowExecution)
		closeDecision.CompleteWorkflowExecutionDecisionAttributes = &s.CompleteWorkflowExecutionDecisionAttributes{
			Result: workflowContext.result,
		}
	}

	if closeDecision != nil {
		decisions = append(decisions, closeDecision)
		elapsed := time.Now().Sub(workflowContext.workflowStartTime)
		metricsScope.Timer(metrics.WorkflowEndToEndLatency).Record(elapsed)
		forceNewDecision = false
	}

	return &s.RespondDecisionTaskCompletedRequest{
		TaskToken:                  task.TaskToken,
		Decisions:                  decisions,
		Identity:                   common.StringPtr(wth.identity),
		ReturnNewDecisionTask:      common.BoolPtr(true),
		ForceCreateNewDecisionTask: common.BoolPtr(forceNewDecision),
	}
}

func errorToFailDecisionTask(taskToken []byte, err error, identity string) *s.RespondDecisionTaskFailedRequest {
	failedCause := s.DecisionTaskFailedCauseWorkflowWorkerUnhandledFailure
	_, details := getErrorDetails(err, nil)
	return &s.RespondDecisionTaskFailedRequest{
		TaskToken: taskToken,
		Cause:     &failedCause,
		Details:   details,
		Identity:  common.StringPtr(identity),
	}
}

func (wth *workflowTaskHandlerImpl) executeAnyPressurePoints(event *s.HistoryEvent, isInReplay bool) error {
	if wth.ppMgr != nil && !reflect.ValueOf(wth.ppMgr).IsNil() && !isInReplay {
		switch event.GetEventType() {
		case s.EventTypeDecisionTaskStarted:
			return wth.ppMgr.Execute(pressurePointTypeDecisionTaskStartTimeout)
		case s.EventTypeActivityTaskScheduled:
			return wth.ppMgr.Execute(pressurePointTypeActivityTaskScheduleTimeout)
		case s.EventTypeActivityTaskStarted:
			return wth.ppMgr.Execute(pressurePointTypeActivityTaskStartTimeout)
		case s.EventTypeDecisionTaskCompleted:
			return wth.ppMgr.Execute(pressurePointTypeDecisionTaskCompleted)
		}
	}
	return nil
}

func newActivityTaskHandler(
	service workflowserviceclient.Interface,
	params workerExecutionParameters,
	env *hostEnvImpl,
) ActivityTaskHandler {
	return newActivityTaskHandlerWithCustomProvider(service, params, env, nil)
}

func newActivityTaskHandlerWithCustomProvider(
	service workflowserviceclient.Interface,
	params workerExecutionParameters,
	env *hostEnvImpl,
	activityProvider activityProvider,
) ActivityTaskHandler {
	return &activityTaskHandlerImpl{
		taskListName:     params.TaskList,
		identity:         params.Identity,
		service:          service,
		logger:           params.Logger,
		metricsScope:     metrics.NewTaggedScope(params.MetricsScope),
		userContext:      params.UserContext,
		hostEnv:          env,
		activityProvider: activityProvider,
		dataConverter:    params.DataConverter,
	}
}

type cadenceInvoker struct {
	sync.Mutex
	identity              string
	service               workflowserviceclient.Interface
	taskToken             []byte
	cancelHandler         func()
	retryPolicy           backoff.RetryPolicy
	heartBeatTimeoutInSec int32       // The heart beat interval configured for this activity.
	hbBatchEndTimer       *time.Timer // Whether we started a batch of operations that need to be reported in the cycle. This gets started on a user call.
	lastDetailsToReport   *[]byte
	closeCh               chan struct{}
}

func (i *cadenceInvoker) Heartbeat(details []byte) error {
	i.Lock()
	defer i.Unlock()

	if i.hbBatchEndTimer != nil {
		// If we have started batching window, keep track of last reported progress.
		i.lastDetailsToReport = &details
		return nil
	}

	isActivityCancelled, err := i.internalHeartBeat(details)

	// If the activity is cancelled, the activity can ignore the cancellation and do its work
	// and complete. Our cancellation is co-operative, so we will try to heartbeat.
	if err == nil || isActivityCancelled {
		// We have successfully sent heartbeat, start next batching window.
		i.lastDetailsToReport = nil

		// Create timer to fire before the threshold to report.
		deadlineToTrigger := i.heartBeatTimeoutInSec
		if deadlineToTrigger <= 0 {
			// If we don't have any heartbeat timeout configured.
			deadlineToTrigger = defaultHeartBeatIntervalInSec
		}

		// We set a deadline at 80% of the timeout.
		duration := time.Duration(0.8*float32(deadlineToTrigger)) * time.Second
		i.hbBatchEndTimer = time.NewTimer(duration)

		go func() {
			select {
			case <-i.hbBatchEndTimer.C:
				// We are close to deadline.
			case <-i.closeCh:
				// We got closed.
				return
			}

			// We close the batch and report the progress.
			var detailsToReport *[]byte

			i.Lock()
			detailsToReport = i.lastDetailsToReport
			i.hbBatchEndTimer.Stop()
			i.hbBatchEndTimer = nil
			i.Unlock()

			if detailsToReport != nil {
				i.Heartbeat(*detailsToReport)
			}
		}()
	}

	return err
}

func (i *cadenceInvoker) internalHeartBeat(details []byte) (bool, error) {
	isActivityCancelled := false
	err := recordActivityHeartbeat(context.Background(), i.service, i.identity, i.taskToken, details, i.retryPolicy)

	switch err.(type) {
	case *CanceledError:
		// We are asked to cancel. inform the activity about cancellation through context.
		i.cancelHandler()
		isActivityCancelled = true

	case *s.EntityNotExistsError:
		// We will pass these through as cancellation for now but something we can change
		// later when we have setter on cancel handler.
		i.cancelHandler()
		isActivityCancelled = true
	}

	// We don't want to bubble temporary errors to the user.
	// This error won't be return to user check RecordActivityHeartbeat().
	return isActivityCancelled, err
}

func (i *cadenceInvoker) Close() {
	i.Lock()
	defer i.Unlock()

	close(i.closeCh)
	if i.hbBatchEndTimer != nil {
		i.hbBatchEndTimer.Stop()
	}
}

func newServiceInvoker(
	taskToken []byte,
	identity string,
	service workflowserviceclient.Interface,
	cancelHandler func(),
	heartBeatTimeoutInSec int32,
) ServiceInvoker {
	return &cadenceInvoker{
		taskToken:             taskToken,
		identity:              identity,
		service:               service,
		cancelHandler:         cancelHandler,
		retryPolicy:           serviceOperationRetryPolicy,
		heartBeatTimeoutInSec: heartBeatTimeoutInSec,
		closeCh:               make(chan struct{}),
	}
}

// Execute executes an implementation of the activity.
func (ath *activityTaskHandlerImpl) Execute(taskList string, t *s.PollForActivityTaskResponse) (result interface{}, err error) {
	traceLog(func() {
		ath.logger.Debug("Processing new activity task",
			zap.String(tagWorkflowID, t.WorkflowExecution.GetWorkflowId()),
			zap.String(tagRunID, t.WorkflowExecution.GetRunId()),
			zap.String(tagActivityType, t.ActivityType.GetName()))
	})

	rootCtx := ath.userContext
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	canCtx, cancel := context.WithCancel(rootCtx)
	invoker := newServiceInvoker(t.TaskToken, ath.identity, ath.service, cancel, t.GetHeartbeatTimeoutSeconds())
	defer invoker.Close()
	ctx := WithActivityTask(canCtx, t, taskList, invoker, ath.logger, ath.metricsScope, ath.dataConverter)
	activityType := *t.ActivityType
	activityImplementation := ath.getActivity(activityType.GetName())
	if activityImplementation == nil {
		// Couldn't find the activity implementation.
		supported := strings.Join(ath.getRegisteredActivityNames(), ", ")
		return nil, fmt.Errorf("unable to find activityType=%v. Supported types: [%v]", activityType.GetName(), supported)
	}

	// panic handler
	defer func() {
		if p := recover(); p != nil {
			topLine := fmt.Sprintf("activity for %s [panic]:", ath.taskListName)
			st := getStackTraceRaw(topLine, 7, 0)
			ath.logger.Error("Activity panic.",
				zap.String("PanicError", fmt.Sprintf("%v", p)),
				zap.String("PanicStack", st))
			scope := ath.metricsScope.GetTaggedScope(tagActivityType, t.ActivityType.GetName())
			scope.Counter(metrics.ActivityTaskPanicCounter).Inc(1)
			panicErr := newPanicError(p, st)
			result, err = convertActivityResultToRespondRequest(ath.identity, t.TaskToken, nil, panicErr, ath.dataConverter), nil
		}
	}()
	info := ctx.Value(activityEnvContextKey).(*activityEnvironment)
	ctx, dlCancelFunc := context.WithDeadline(ctx, info.deadline)

	output, err := activityImplementation.Execute(ctx, t.Input)

	dlCancelFunc()
	if <-ctx.Done(); ctx.Err() == context.DeadlineExceeded {
		return nil, ctx.Err()
	}

	return convertActivityResultToRespondRequest(ath.identity, t.TaskToken, output, err, ath.dataConverter), nil
}

func (ath *activityTaskHandlerImpl) getActivity(name string) activity {
	if ath.activityProvider != nil {
		return ath.activityProvider(name)
	}

	if a, ok := ath.hostEnv.getActivity(name); ok {
		return a
	}

	return nil
}

func (ath *activityTaskHandlerImpl) getRegisteredActivityNames() (activityNames []string) {
	for _, a := range ath.hostEnv.activityFuncMap {
		activityNames = append(activityNames, a.ActivityType().Name)
	}
	return
}

func createNewDecision(decisionType s.DecisionType) *s.Decision {
	return &s.Decision{
		DecisionType: common.DecisionTypePtr(decisionType),
	}
}

func recordActivityHeartbeat(
	ctx context.Context,
	service workflowserviceclient.Interface,
	identity string,
	taskToken, details []byte,
	retryPolicy backoff.RetryPolicy,
) error {
	request := &s.RecordActivityTaskHeartbeatRequest{
		TaskToken: taskToken,
		Details:   details,
		Identity:  common.StringPtr(identity)}

	var heartbeatResponse *s.RecordActivityTaskHeartbeatResponse
	heartbeatErr := backoff.Retry(ctx,
		func() error {
			tchCtx, cancel, opt := newChannelContext(ctx)
			defer cancel()

			var err error
			heartbeatResponse, err = service.RecordActivityTaskHeartbeat(tchCtx, request, opt...)
			return err
		}, retryPolicy, isServiceTransientError)

	if heartbeatErr == nil && heartbeatResponse != nil && heartbeatResponse.GetCancelRequested() {
		return NewCanceledError()
	}

	return heartbeatErr
}

func recordActivityHeartbeatByID(
	ctx context.Context,
	service workflowserviceclient.Interface,
	identity string,
	domain, workflowID, runID, activityID string,
	details []byte,
	retryPolicy backoff.RetryPolicy,
) error {
	request := &s.RecordActivityTaskHeartbeatByIDRequest{
		Domain:     common.StringPtr(domain),
		WorkflowID: common.StringPtr(workflowID),
		RunID:      common.StringPtr(runID),
		ActivityID: common.StringPtr(activityID),
		Details:    details,
		Identity:   common.StringPtr(identity)}

	var heartbeatResponse *s.RecordActivityTaskHeartbeatResponse
	heartbeatErr := backoff.Retry(ctx,
		func() error {
			tchCtx, cancel, opt := newChannelContext(ctx)
			defer cancel()

			var err error
			heartbeatResponse, err = service.RecordActivityTaskHeartbeatByID(tchCtx, request, opt...)
			return err
		}, retryPolicy, isServiceTransientError)

	if heartbeatErr == nil && heartbeatResponse != nil && heartbeatResponse.GetCancelRequested() {
		return NewCanceledError()
	}

	return heartbeatErr
}

// This enables verbose logging in the client library.
// check worker.EnableVerboseLogging()
func traceLog(fn func()) {
	if enableVerboseLogging {
		fn()
	}
}
