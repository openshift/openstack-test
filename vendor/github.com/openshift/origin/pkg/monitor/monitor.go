package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/openshift/origin/pkg/monitor/monitorapi"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
)

// Monitor records events that have occurred in memory and can also periodically
// sample results.
type Monitor struct {
	interval time.Duration
	samplers []SamplerFunc

	lock           sync.Mutex
	events         monitorapi.Intervals
	unsortedEvents monitorapi.Intervals
	samples        []*sample

	recordedResourceLock sync.Mutex
	recordedResources    monitorapi.ResourcesMap
}

// NewMonitor creates a monitor with the default sampling interval.
func NewMonitor() *Monitor {
	return NewMonitorWithInterval(15 * time.Second)
}

// NewMonitorWithInterval creates a monitor that samples at the provided
// interval.
func NewMonitorWithInterval(interval time.Duration) *Monitor {
	return &Monitor{
		interval:          interval,
		recordedResources: monitorapi.ResourcesMap{},
	}
}

var _ Interface = &Monitor{}

// StartSampling starts sampling every interval until the provided context is done.
// A sample is captured when the context is closed.
func (m *Monitor) StartSampling(ctx context.Context) {
	if m.interval == 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		hasConditions := false
		for {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				hasConditions = m.sample(hasConditions)
				return
			}
			hasConditions = m.sample(hasConditions)
		}
	}()
}

// AddSampler adds a sampler function to the list of samplers to run every interval.
// Conditions discovered this way are recorded with a start and end time if they persist
// across multiple sampling intervals.
func (m *Monitor) AddSampler(fn SamplerFunc) {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.samplers = append(m.samplers, fn)
}

func (m *Monitor) CurrentResourceState() monitorapi.ResourcesMap {
	m.recordedResourceLock.Lock()
	defer m.recordedResourceLock.Unlock()

	ret := monitorapi.ResourcesMap{}
	for resourceType, instanceResourceMap := range m.recordedResources {
		retInstance := monitorapi.InstanceMap{}
		for instanceKey, obj := range instanceResourceMap {
			retInstance[instanceKey] = obj.DeepCopyObject()
		}
		ret[resourceType] = retInstance
	}

	return ret
}

func (m *Monitor) RecordResource(resourceType string, obj runtime.Object) {
	m.recordedResourceLock.Lock()
	defer m.recordedResourceLock.Unlock()

	recordedResource, ok := m.recordedResources[resourceType]
	if !ok {
		recordedResource = monitorapi.InstanceMap{}
		m.recordedResources[resourceType] = recordedResource
	}

	newMetadata, err := meta.Accessor(obj)
	if err != nil {
		// coding error
		panic(err)
	}
	key := monitorapi.InstanceKey{
		Namespace: newMetadata.GetNamespace(),
		Name:      newMetadata.GetName(),
		UID:       fmt.Sprintf("%v", newMetadata.GetUID()),
	}

	toStore := obj.DeepCopyObject()
	// without metadata, just stomp in the new value, we can't add annotations
	if newMetadata == nil {
		recordedResource[key] = toStore
		return
	}

	newAnnotations := newMetadata.GetAnnotations()
	if newAnnotations == nil {
		newAnnotations = map[string]string{}
	}
	existingResource, ok := recordedResource[key]
	if !ok {
		if newMetadata != nil {
			newAnnotations[monitorapi.ObservedUpdateCountAnnotation] = "1"
			newAnnotations[monitorapi.ObservedRecreationCountAnnotation] = "0"
			newMetadata.SetAnnotations(newAnnotations)
		}
		recordedResource[key] = toStore
		return
	}

	existingMetadata, _ := meta.Accessor(existingResource)
	// without metadata, just stomp in the new value, we can't add annotations
	if existingMetadata == nil {
		recordedResource[key] = toStore
		return
	}

	existingAnnotations := existingMetadata.GetAnnotations()
	if existingAnnotations == nil {
		existingAnnotations = map[string]string{}
	}
	existingUpdateCountStr := existingAnnotations[monitorapi.ObservedUpdateCountAnnotation]
	if existingUpdateCount, err := strconv.ParseInt(existingUpdateCountStr, 10, 32); err != nil {
		newAnnotations[monitorapi.ObservedUpdateCountAnnotation] = "1"
	} else {
		newAnnotations[monitorapi.ObservedUpdateCountAnnotation] = fmt.Sprintf("%d", existingUpdateCount+1)
	}

	// set the recreate count. increment if the UIDs don't match
	existingRecreateCountStr := existingAnnotations[monitorapi.ObservedUpdateCountAnnotation]
	if existingMetadata.GetUID() != newMetadata.GetUID() {
		if existingRecreateCount, err := strconv.ParseInt(existingRecreateCountStr, 10, 32); err != nil {
			newAnnotations[monitorapi.ObservedRecreationCountAnnotation] = existingRecreateCountStr
		} else {
			newAnnotations[monitorapi.ObservedRecreationCountAnnotation] = fmt.Sprintf("%d", existingRecreateCount+1)
		}
	} else {
		newAnnotations[monitorapi.ObservedRecreationCountAnnotation] = existingRecreateCountStr
	}

	newMetadata.SetAnnotations(newAnnotations)
	recordedResource[key] = toStore
	return
}

// Record captures one or more conditions at the current time. All conditions are recorded
// in monotonic order as EventInterval objects.
func (m *Monitor) Record(conditions ...monitorapi.Condition) {
	if len(conditions) == 0 {
		return
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	t := time.Now().UTC()
	for _, condition := range conditions {
		m.events = append(m.events, monitorapi.EventInterval{
			Condition: condition,
			From:      t,
			To:        t,
		})
	}
}

// StartInterval inserts a record at time t with the provided condition and returns an opaque
// locator to the interval. The caller may close the sample at any point by invoking EndInterval().
func (m *Monitor) StartInterval(t time.Time, condition monitorapi.Condition) int {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.unsortedEvents = append(m.unsortedEvents, monitorapi.EventInterval{
		Condition: condition,
		From:      t,
	})
	return len(m.unsortedEvents) - 1
}

// EndInterval updates the To of the interval started by StartInterval if t is greater than
// the from.
func (m *Monitor) EndInterval(startedInterval int, t time.Time) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if startedInterval < len(m.unsortedEvents) {
		if m.unsortedEvents[startedInterval].From.Before(t) {
			m.unsortedEvents[startedInterval].To = t
		}
	}
}

// RecordAt captures one or more conditions at the provided time. All conditions are recorded
// as EventInterval objects.
func (m *Monitor) RecordAt(t time.Time, conditions ...monitorapi.Condition) {
	if len(conditions) == 0 {
		return
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	for _, condition := range conditions {
		m.unsortedEvents = append(m.unsortedEvents, monitorapi.EventInterval{
			Condition: condition,
			From:      t,
			To:        t,
		})
	}
}

func (m *Monitor) sample(hasPrevious bool) bool {
	m.lock.Lock()
	samplers := m.samplers
	m.lock.Unlock()

	now := time.Now().UTC()
	var conditions []*monitorapi.Condition
	for _, fn := range samplers {
		conditions = append(conditions, fn(now)...)
	}
	if len(conditions) == 0 {
		if !hasPrevious {
			return false
		}
	}

	m.lock.Lock()
	defer m.lock.Unlock()
	m.samples = append(m.samples, &sample{
		at:         now,
		conditions: conditions,
	})
	return len(conditions) > 0
}

func (m *Monitor) snapshot() ([]*sample, monitorapi.Intervals, monitorapi.Intervals) {
	m.lock.Lock()
	defer m.lock.Unlock()
	return m.samples, m.events, m.unsortedEvents
}

// Conditions returns all conditions that were sampled in the interval
// between from and to. If that does not include a sample interval, no
// results will be returned. Intervals are returned in order of
// their first sampling. A condition that was only sampled once is
// returned with from == to. No duplicate conditions are returned
// unless a sampling interval did not report that value.
func (m *Monitor) Conditions(from, to time.Time) monitorapi.Intervals {
	samples, _, _ := m.snapshot()
	return filterSamples(samples, from, to)
}

// Intervals returns all events that occur between from and to, including
// any sampled conditions that were encountered during that period.
// Intervals are returned in order of their occurrence. The returned slice
// is a copy of the monitor's state and is safe to update.
func (m *Monitor) Intervals(from, to time.Time) monitorapi.Intervals {
	samples, sortedEvents, unsortedEvents := m.snapshot()

	intervals := mergeIntervals(sortedEvents.Slice(from, to), unsortedEvents.CopyAndSort(from, to), filterSamples(samples, from, to))

	return intervals
}

// filterSamples converts the sorted samples that are within [from,to) to a set of
// intervals.
// TODO: simplify this by having the monitor samplers produce intervals themselves
//
//	and make the streaming print logic simply show transitions.
func filterSamples(samples []*sample, from, to time.Time) monitorapi.Intervals {
	if len(samples) == 0 {
		return nil
	}

	if !from.IsZero() {
		first := sort.Search(len(samples), func(i int) bool {
			return samples[i].at.After(from)
		})
		if first == -1 {
			return nil
		}
		samples = samples[first:]
	}

	if !to.IsZero() {
		for i, sample := range samples {
			if sample.at.After(to) {
				samples = samples[:i]
				break
			}
		}
	}
	if len(samples) == 0 {
		return nil
	}

	intervals := make(monitorapi.Intervals, 0, len(samples)*2)
	last, next := make(map[monitorapi.Condition]*monitorapi.EventInterval), make(map[monitorapi.Condition]*monitorapi.EventInterval)
	for _, sample := range samples {
		for _, condition := range sample.conditions {
			interval, ok := last[*condition]
			if ok {
				interval.To = sample.at
				next[*condition] = interval
				continue
			}
			intervals = append(intervals, monitorapi.EventInterval{
				Condition: *condition,
				From:      sample.at,
				To:        sample.at.Add(time.Second),
			})
			next[*condition] = &intervals[len(intervals)-1]
		}
		for k := range last {
			delete(last, k)
		}
		last, next = next, last
	}
	return intervals
}

// mergeEvents returns a sorted list of all events provided as sources. This could be
// more efficient by requiring all sources to be sorted and then performing a zipper
// merge.
func mergeIntervals(sets ...monitorapi.Intervals) monitorapi.Intervals {
	total := 0
	for _, set := range sets {
		total += len(set)
	}
	merged := make(monitorapi.Intervals, 0, total)
	for _, set := range sets {
		merged = append(merged, set...)
	}
	sort.Sort(merged)
	return merged
}
