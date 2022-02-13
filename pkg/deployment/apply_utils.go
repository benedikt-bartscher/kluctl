package deployment

import (
	"fmt"
	"github.com/codablock/kluctl/pkg/diff"
	"github.com/codablock/kluctl/pkg/k8s"
	"github.com/codablock/kluctl/pkg/types"
	"github.com/codablock/kluctl/pkg/utils"
	"github.com/codablock/kluctl/pkg/utils/uo"
	"github.com/codablock/kluctl/pkg/validation"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sync"
	"time"
)

type applyUtilOptions struct {
	forceApply          bool
	replaceOnError      bool
	forceReplaceOnError bool
	dryRun              bool
	abortOnError        bool
	hookTimeout         time.Duration
}

type applyUtil struct {
	deploymentCollection *DeploymentCollection
	k                    *k8s.K8sCluster
	o                    applyUtilOptions

	appliedObjects     map[types.ObjectRef]*unstructured.Unstructured
	appliedHookObjects map[types.ObjectRef]*unstructured.Unstructured
	abortSignal        bool
	mutex              sync.Mutex
}

func newApplyUtil(deploymentCollection *DeploymentCollection, k *k8s.K8sCluster, o applyUtilOptions) *applyUtil {
	return &applyUtil{
		deploymentCollection: deploymentCollection,
		k:                    k,
		o:                    o,
		appliedObjects:       map[types.ObjectRef]*unstructured.Unstructured{},
		appliedHookObjects:   map[types.ObjectRef]*unstructured.Unstructured{},
	}
}

func (a *applyUtil) handleResult(appliedObject *unstructured.Unstructured, hook bool) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	ref := types.RefFromObject(appliedObject)
	if hook {
		a.appliedHookObjects[ref] = appliedObject
	} else {
		a.appliedObjects[ref] = appliedObject
	}
}

func (a *applyUtil) handleApiWarnings(ref types.ObjectRef, warnings []k8s.ApiWarning) {
	a.deploymentCollection.addApiWarnings(ref, warnings)
}

func (a *applyUtil) handleWarning(ref types.ObjectRef, warning error) {
	a.deploymentCollection.addWarning(ref, warning)
}

func (a *applyUtil) handleError(ref types.ObjectRef, err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if a.o.abortOnError {
		a.abortSignal = true
	}

	a.deploymentCollection.addError(ref, err)
}

func (a *applyUtil) hadError(ref types.ObjectRef) bool {
	return a.deploymentCollection.hadError(ref)
}

func (a *applyUtil) deleteObject(ref types.ObjectRef) bool {
	o := k8s.DeleteOptions{
		ForceDryRun: a.o.dryRun,
	}
	apiWarnings, err := a.k.DeleteSingleObject(ref, o)
	a.handleApiWarnings(ref, apiWarnings)
	if err != nil {
		a.handleError(ref, err)
		return false
	}
	return true
}

func (a *applyUtil) retryApplyForceReplace(x *unstructured.Unstructured, hook bool, applyError error) {
	ref := types.RefFromObject(x)
	log2 := log.WithField("ref", ref)

	if !a.o.forceReplaceOnError {
		a.handleError(ref, applyError)
		return
	}

	log2.Warningf("Patching failed, retrying by deleting and re-applying")

	if !a.deleteObject(ref) {
		return
	}

	if !a.o.dryRun {
		o := k8s.PatchOptions{
			ForceDryRun: a.o.dryRun,
		}
		r, apiWarnings, err := a.k.PatchObject(x, o)
		a.handleApiWarnings(ref, apiWarnings)
		if err != nil {
			a.handleError(ref, err)
			return
		}
		a.handleResult(r, hook)
	} else {
		a.handleResult(x, hook)
	}
}

func (a *applyUtil) retryApplyWithReplace(x *unstructured.Unstructured, hook bool, remoteObject *unstructured.Unstructured, applyError error) {
	ref := types.RefFromObject(x)
	log2 := log.WithField("ref", ref)

	if !a.o.replaceOnError || remoteObject == nil {
		a.handleError(ref, applyError)
		return
	}

	log2.Warningf("Patching failed, retrying with replace instead of patch")

	rv := remoteObject.GetResourceVersion()
	x2 := uo.CopyUnstructured(x)
	x2.SetResourceVersion(rv)

	o := k8s.UpdateOptions{
		ForceDryRun: a.o.dryRun,
	}

	r, apiWarnings, err := a.k.UpdateObject(x, o)
	a.handleApiWarnings(ref, apiWarnings)
	if err != nil {
		a.retryApplyForceReplace(x, hook, err)
		return
	}
	a.handleResult(r, hook)
}

func (a *applyUtil) retryApplyWithConflicts(x *unstructured.Unstructured, hook bool, remoteObject *unstructured.Unstructured, applyError error) {
	ref := types.RefFromObject(x)

	if remoteObject == nil {
		a.handleError(ref, applyError)
	}

	var x2 *unstructured.Unstructured
	if !a.o.forceApply {
		statusError, ok := applyError.(*errors.StatusError)
		if !ok {
			a.handleError(ref, applyError)
			return
		}

		x3, lostOwnership, err := diff.ResolveFieldManagerConflicts(x, remoteObject, statusError.ErrStatus)
		if err != nil {
			a.handleError(ref, err)
			return
		}
		for _, lo := range lostOwnership {
			a.deploymentCollection.addWarning(ref, fmt.Errorf("%s. Not updating field '%s' as we lost field ownership", lo.Message, lo.Field))
		}
		x2 = x3
	} else {
		x2 = x
	}

	options := k8s.PatchOptions{
		ForceDryRun: a.o.dryRun,
		ForceApply:  true,
	}
	r, apiWarnings, err := a.k.PatchObject(x2, options)
	a.handleApiWarnings(ref, apiWarnings)
	if err != nil {
		// We didn't manage to solve it, better to abort (and not retry with replace!)
		a.handleError(ref, err)
		return
	}
	a.handleResult(r, hook)
}

func (a *applyUtil) applyObject(x *unstructured.Unstructured, replaced bool, hook bool) {
	ref := types.RefFromObject(x)
	log2 := log.WithField("ref", ref)
	log2.Debugf("applying object")

	x = a.k.FixObjectForPatch(x)
	remoteObject := a.deploymentCollection.getRemoteObject(ref)

	if a.o.dryRun && replaced && remoteObject != nil {
		// Let's simulate that this object was deleted in dry-run mode. If we'd actually try a dry-run apply with
		// this object, it might fail as it is expected to not exist.
		a.handleResult(x, hook)
		return
	}

	options := k8s.PatchOptions{
		ForceDryRun: a.o.dryRun,
	}
	r, apiWarnings, err := a.k.PatchObject(x, options)
	a.handleApiWarnings(ref, apiWarnings)
	if err == nil {
		a.handleResult(r, hook)
	} else if meta.IsNoMatchError(err) {
		a.handleError(ref, err)
	} else if errors.IsConflict(err) {
		a.retryApplyWithConflicts(x, hook, remoteObject, err)
	} else {
		a.retryApplyWithReplace(x, hook, remoteObject, err)
	}
}

func (a *applyUtil) waitHook(ref types.ObjectRef) bool {
	if a.o.dryRun {
		return true
	}

	log2 := log.WithField("ref", ref)
	log2.Debugf("Waiting for hook to get ready")

	didLog := false
	startTime := time.Now()
	for true {
		o, apiWarnings, err := a.k.GetSingleObject(ref)
		a.handleApiWarnings(ref, apiWarnings)
		if err != nil {
			if errors.IsNotFound(err) {
				if didLog {
					log2.Warningf("Cancelled waiting for hook as it disappeared while waiting for it")
				}
				a.handleError(ref, fmt.Errorf("object disappeared while waiting for it to become ready"))
				return false
			}
			a.handleError(ref, err)
			return false
		}
		v := validation.ValidateObject(o, false)
		if v.Ready {
			if didLog {
				log2.Infof("Finished waiting for hook")
			}
			return true
		}
		if len(v.Errors) != 0 {
			if didLog {
				log2.Warningf("Cancelled waiting for hook due to errors")
			}
			for _, e := range v.Errors {
				a.handleError(ref, fmt.Errorf(e.Error))
			}
			return false
		}

		if a.o.hookTimeout != 0 && time.Now().Sub(startTime) >= a.o.hookTimeout {
			err := fmt.Errorf("timed out while waiting for hook")
			log2.Warningf(err.Error())
			a.handleError(ref, err)
			return false
		}

		if !didLog {
			log2.Infof("Waiting for hook to get ready...")
			didLog = true
		}

		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func (a *applyUtil) applyKustomizeDeployment(d *deploymentItem) {
	if d.config.Path == nil {
		return
	}

	if !d.checkInclusionForDeploy() {
		a.doLog(d, log.InfoLevel, "Skipping")
		return
	}

	initialDeploy := true
	for _, o := range d.objects {
		if a.deploymentCollection.getRemoteObject(types.RefFromObject(o)) != nil {
			initialDeploy = false
		}
	}

	h := hooksUtil{a: a}

	if initialDeploy {
		h.runHooks(d, []string{"pre-deploy-initial", "pre-deploy"})
	} else {
		h.runHooks(d, []string{"pre-deploy-upgrade", "pre-deploy"})
	}

	var applyObjects []*unstructured.Unstructured
	for _, o := range d.objects {
		if h.getHook(o) != nil {
			continue
		}
		applyObjects = append(applyObjects, o)
	}

	a.doLog(d, log.InfoLevel, "Applying %d objects", len(d.objects))
	for _, o := range applyObjects {
		a.applyObject(o, false, false)
	}

	if initialDeploy {
		h.runHooks(d, []string{"post-deploy-initial", "post-deploy"})
	} else {
		h.runHooks(d, []string{"post-deploy-upgrade", "post-deploy"})
	}
}

func (a *applyUtil) applyDeployments() {
	log.Infof("Running server-side apply for all objects")

	wp := utils.NewDebuggerAwareWorkerPool(16)
	defer wp.StopWait(false)

	previousWasBarrier := false
	for _, d_ := range a.deploymentCollection.deployments {
		d := d_
		if a.abortSignal {
			break
		}
		if previousWasBarrier {
			log.Infof("Waiting on barrier...")
			_ = wp.StopWait(true)
		}

		previousWasBarrier = d.config.Barrier != nil && *d.config.Barrier

		wp.Submit(func() error {
			a.applyKustomizeDeployment(d)
			return nil
		})
	}
	_ = wp.StopWait(false)
}

func (a *applyUtil) doLog(d *deploymentItem, level log.Level, s string, f ...interface{}) {
	s = fmt.Sprintf("%s: %s", d.relToProjectItemDir, fmt.Sprintf(s, f...))
	log.StandardLogger().Logf(level, s)
}
