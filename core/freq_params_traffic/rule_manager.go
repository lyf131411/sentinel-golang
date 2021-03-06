package freq_params_traffic

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/alibaba/sentinel-golang/logging"
	"github.com/pkg/errors"
)

// TrafficControllerGenFunc represents the TrafficShapingController generator function of a specific control behavior.
type TrafficControllerGenFunc func(r *Rule, reuseMetric *ParamsMetric) TrafficShapingController

// trafficControllerMap represents the map storage for TrafficShapingController.
type trafficControllerMap map[string][]TrafficShapingController

var (
	logger = logging.GetDefaultLogger()

	tcGenFuncMap = make(map[ControlBehavior]TrafficControllerGenFunc)
	tcMap        = make(trafficControllerMap)
	tcMux        = new(sync.RWMutex)
)

func init() {
	// Initialize the traffic shaping controller generator map for existing control behaviors.
	tcGenFuncMap[Reject] = func(r *Rule, reuseMetric *ParamsMetric) TrafficShapingController {
		var baseTc *baseTrafficShapingController
		if reuseMetric != nil {
			// new BaseTrafficShapingController with reuse statistic metric
			baseTc = newBaseTrafficShapingControllerWithMetric(r, reuseMetric)
		} else {
			baseTc = newBaseTrafficShapingController(r)
		}
		return &rejectTrafficShapingController{
			baseTrafficShapingController: *baseTc,
			burstCount:                   r.BurstCount,
		}
	}

	tcGenFuncMap[Throttling] = func(r *Rule, reuseMetric *ParamsMetric) TrafficShapingController {
		var baseTc *baseTrafficShapingController
		if reuseMetric != nil {
			baseTc = newBaseTrafficShapingControllerWithMetric(r, reuseMetric)
		} else {
			baseTc = newBaseTrafficShapingController(r)
		}
		return &throttlingTrafficShapingController{
			baseTrafficShapingController: *baseTc,
			maxQueueingTimeMs:            r.MaxQueueingTimeMs,
		}
	}
}

func getTrafficControllersFor(res string) []TrafficShapingController {
	tcMux.RLock()
	defer tcMux.RUnlock()

	return tcMap[res]
}

// LoadRules replaces old rules with the given frequency parameters flow control rules.
// return value:
//
// bool: was designed to indicate whether the internal map has been changed
// error: was designed to indicate whether occurs the error.
func LoadRules(rules []*Rule) (bool, error) {
	err := onRuleUpdate(rules)
	return true, err
}

// GetRules return the whole of rules
func GetRules() []*Rule {
	tcMux.RLock()
	defer tcMux.RUnlock()

	return rulesFrom(tcMap)
}

// ClearRules clears all rules in frequency parameters flow control components
func ClearRules() error {
	_, err := LoadRules(nil)
	return err
}

func onRuleUpdate(rules []*Rule) (err error) {
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("%+v", r)
			}
		}
	}()

	tcMux.Lock()
	defer tcMux.Unlock()

	m := buildTcMap(rules)
	tcMap = m

	logRuleUpdate(m)
	return nil
}

func logRuleUpdate(m trafficControllerMap) {
	s, err := json.Marshal(m)
	if err != nil {
		logger.Info("Frequency parameters flow control rules loaded")
	} else {
		logger.Infof("Frequency parameters flow control rules loaded: %s", s)
	}
}

func rulesFrom(m trafficControllerMap) []*Rule {
	rules := make([]*Rule, 0)
	if len(m) == 0 {
		return rules
	}
	for _, rs := range m {
		if len(rs) == 0 {
			continue
		}
		for _, r := range rs {
			if r != nil && r.BoundRule() != nil {
				rules = append(rules, r.BoundRule())
			}
		}
	}
	return rules
}

func calculateReuseIndexFor(r *Rule, oldResTcs []TrafficShapingController) (equalIdx, reuseStatIdx int) {
	// the index of equivalent rule in old traffic shaping controller slice
	equalIdx = -1
	// the index of statistic reusable rule in old traffic shaping controller slice
	reuseStatIdx = -1

	for idx, oldTc := range oldResTcs {
		oldRule := oldTc.BoundRule()
		if oldRule.Equals(r) {
			// break if there is equivalent rule
			equalIdx = idx
			break
		}
		// find the index of first StatReusable rule
		if !oldRule.IsStatReusable(r) {
			continue
		}
		if reuseStatIdx >= 0 {
			// had find reuse rule.
			continue
		}
		reuseStatIdx = idx
	}
	return equalIdx, reuseStatIdx
}

func insertTcToTcMap(tc TrafficShapingController, res string, m trafficControllerMap) {
	tcsOfRes, exists := m[res]
	if !exists {
		tcsOfRes = make([]TrafficShapingController, 0, 1)
		m[res] = append(tcsOfRes, tc)
	} else {
		m[res] = append(tcsOfRes, tc)
	}
}

// buildTcMap be called on the condition that the mutex is locked
func buildTcMap(rules []*Rule) trafficControllerMap {
	m := make(trafficControllerMap)
	if len(rules) == 0 {
		return m
	}

	for _, r := range rules {
		if err := IsValidRule(r); err != nil {
			logger.Warnf("Ignoring invalid frequency params Rule: %+v, reason: %s", r, err.Error())
			continue
		}

		res := r.Resource
		oldResTcs := tcMap[res]
		equalIdx, reuseStatIdx := calculateReuseIndexFor(r, oldResTcs)

		// there is equivalent rule in old traffic shaping controller slice
		if equalIdx >= 0 {
			equalOldTC := oldResTcs[equalIdx]
			insertTcToTcMap(equalOldTC, res, m)
			// remove old tc from old resTcs
			tcMap[res] = append(oldResTcs[:equalIdx], oldResTcs[equalIdx+1:]...)
			continue
		}

		// generate new traffic shaping controller
		generator, supported := tcGenFuncMap[r.Behavior]
		if !supported {
			logger.Warnf("Ignoring the frequency params Rule due to unsupported control strategy: %+v", r)
			continue
		}
		var tc TrafficShapingController
		if reuseStatIdx >= 0 {
			// generate new traffic shaping controller with reusable statistic metric.
			tc = generator(r, oldResTcs[reuseStatIdx].BoundMetric())
		} else {
			tc = generator(r, nil)
		}
		if tc == nil {
			logger.Debugf("Ignoring the frequency params Rule due to bad generated traffic controller: %+v", r)
			continue
		}

		//  remove the reused traffic shaping controller old res tcs
		if reuseStatIdx >= 0 {
			tcMap[res] = append(oldResTcs[:reuseStatIdx], oldResTcs[reuseStatIdx+1:]...)
		}
		insertTcToTcMap(tc, res, m)
	}
	return m
}

func IsValidRule(rule *Rule) error {
	if rule == nil {
		return errors.New("nil freq params Rule")
	}
	if len(rule.Resource) == 0 {
		return errors.New("empty resource name")
	}
	if rule.Threshold < 0 {
		return errors.New("negative threshold")
	}
	if rule.MetricType < 0 {
		return errors.New("invalid metric type")
	}
	if rule.Behavior < 0 {
		return errors.New("invalid control strategy")
	}
	if rule.ParamIndex < 0 {
		return errors.New("invalid param index")
	}
	if rule.DurationInSec < 0 {
		return errors.New("invalid duration")
	}
	return checkControlBehaviorField(rule)
}

func checkControlBehaviorField(rule *Rule) error {
	switch rule.Behavior {
	case Reject:
		if rule.BurstCount < 0 {
			return errors.New("invalid BurstCount")
		}
		return nil
	case Throttling:
		if rule.MaxQueueingTimeMs < 0 {
			return errors.New("invalid MaxQueueingTimeMs")
		}
		return nil
	default:
	}
	return nil
}

// SetTrafficShapingGenerator sets the traffic controller generator for the given control behavior.
// Note that modifying the generator of default control behaviors is not allowed.
func SetTrafficShapingGenerator(cb ControlBehavior, generator TrafficControllerGenFunc) error {
	if generator == nil {
		return errors.New("nil generator")
	}
	if cb >= Reject && cb <= Throttling {
		return errors.New("not allowed to replace the generator for default control behaviors")
	}
	tcMux.Lock()
	defer tcMux.Unlock()

	tcGenFuncMap[cb] = generator
	return nil
}

func RemoveTrafficShapingGenerator(cb ControlBehavior) error {
	if cb >= Reject && cb <= Throttling {
		return errors.New("not allowed to replace the generator for default control behaviors")
	}
	tcMux.Lock()
	defer tcMux.Unlock()

	delete(tcGenFuncMap, cb)
	return nil
}
