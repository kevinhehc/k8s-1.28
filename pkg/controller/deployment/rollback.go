/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package deployment

import (
	"context"
	"fmt"
	"strconv"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	deploymentutil "k8s.io/kubernetes/pkg/controller/deployment/util"
)

// rollback the deployment to the specified revision. In any case cleanup the rollback spec.
/*
1、获取 newRS 和 oldRSs；
2、调用 getRollbackTo 获取 rollback 的 revision；
3、判断 revision 以及对应的 rs 是否存在，若 revision 为 0，则表示回滚到上一个版本；
4、若存在对应的 rs，则调用 rollbackToTemplate 方法将 rs.Spec.Template 赋值给 d.Spec.Template，否则放弃回滚操作；
*/
func (dc *DeploymentController) rollback(ctx context.Context, d *apps.Deployment, rsList []*apps.ReplicaSet) error {
	logger := klog.FromContext(ctx)
	// 1、获取 newRS 和 oldRSs
	newRS, allOldRSs, err := dc.getAllReplicaSetsAndSyncRevision(ctx, d, rsList, true)
	if err != nil {
		return err
	}

	allRSs := append(allOldRSs, newRS)
	// 2、调用 getRollbackTo 获取 rollback 的 revision
	rollbackTo := getRollbackTo(d)
	// If rollback revision is 0, rollback to the last revision
	// 3、判断 revision 以及对应的 rs 是否存在，若 revision 为 0，则表示回滚到最新的版本
	if rollbackTo.Revision == 0 {
		if rollbackTo.Revision = deploymentutil.LastRevision(logger, allRSs); rollbackTo.Revision == 0 {
			// If we still can't find the last revision, gives up rollback
			dc.emitRollbackWarningEvent(d, deploymentutil.RollbackRevisionNotFound, "Unable to find last revision.")
			// Gives up rollback
			// 4、清除回滚标志放弃回滚操作
			return dc.updateDeploymentAndClearRollbackTo(ctx, d)
		}
	}
	for _, rs := range allRSs {
		v, err := deploymentutil.Revision(rs)
		if err != nil {
			logger.V(4).Info("Unable to extract revision from deployment's replica set", "replicaSet", klog.KObj(rs), "err", err)
			continue
		}
		if v == rollbackTo.Revision {
			// 5、调用 rollbackToTemplate 进行回滚操作
			logger.V(4).Info("Found replica set with desired revision", "replicaSet", klog.KObj(rs), "revision", v)
			// rollback by copying podTemplate.Spec from the replica set
			// revision number will be incremented during the next getAllReplicaSetsAndSyncRevision call
			// no-op if the spec matches current deployment's podTemplate.Spec
			performedRollback, err := dc.rollbackToTemplate(ctx, d, rs)
			if performedRollback && err == nil {
				dc.emitRollbackNormalEvent(d, fmt.Sprintf("Rolled back deployment %q to revision %d", d.Name, rollbackTo.Revision))
			}
			return err
		}
	}
	dc.emitRollbackWarningEvent(d, deploymentutil.RollbackRevisionNotFound, "Unable to find the revision to rollback to.")
	// Gives up rollback
	return dc.updateDeploymentAndClearRollbackTo(ctx, d)
}

// rollbackToTemplate compares the templates of the provided deployment and replica set and
// updates the deployment with the replica set template in case they are different. It also
// cleans up the rollback spec so subsequent requeues of the deployment won't end up in here.
/*
rollbackToTemplate 会判断 deployment.Spec.Template 和 rs.Spec.Template 是否相等，若相等则无需回滚，
否则使用 rs.Spec.Template 替换 deployment.Spec.Template，然后更新 deployment 的 spec 并清除回滚标志。
*/
func (dc *DeploymentController) rollbackToTemplate(ctx context.Context, d *apps.Deployment, rs *apps.ReplicaSet) (bool, error) {
	logger := klog.FromContext(ctx)
	performedRollback := false
	// 1、比较 d.Spec.Template 和 rs.Spec.Template 是否相等
	if !deploymentutil.EqualIgnoreHash(&d.Spec.Template, &rs.Spec.Template) {
		logger.V(4).Info("Rolling back deployment to old template spec", "deployment", klog.KObj(d), "templateSpec", rs.Spec.Template.Spec)
		// 2、替换 d.Spec.Template
		deploymentutil.SetFromReplicaSetTemplate(d, rs.Spec.Template)
		// set RS (the old RS we'll rolling back to) annotations back to the deployment;
		// otherwise, the deployment's current annotations (should be the same as current new RS) will be copied to the RS after the rollback.
		//
		// For example,
		// A Deployment has old RS1 with annotation {change-cause:create}, and new RS2 {change-cause:edit}.
		// Note that both annotations are copied from Deployment, and the Deployment should be annotated {change-cause:edit} as well.
		// Now, rollback Deployment to RS1, we should update Deployment's pod-template and also copy annotation from RS1.
		// Deployment is now annotated {change-cause:create}, and we have new RS1 {change-cause:create}, old RS2 {change-cause:edit}.
		//
		// If we don't copy the annotations back from RS to deployment on rollback, the Deployment will stay as {change-cause:edit},
		// and new RS1 becomes {change-cause:edit} (copied from deployment after rollback), old RS2 {change-cause:edit}, which is not correct.
		// 3、设置 annotation
		deploymentutil.SetDeploymentAnnotationsTo(d, rs)
		performedRollback = true
	} else {
		logger.V(4).Info("Rolling back to a revision that contains the same template as current deployment, skipping rollback...", "deployment", klog.KObj(d))
		eventMsg := fmt.Sprintf("The rollback revision contains the same template as current deployment %q", d.Name)
		dc.emitRollbackWarningEvent(d, deploymentutil.RollbackTemplateUnchanged, eventMsg)
	}

	// 4、更新 deployment 并清除回滚标志
	return performedRollback, dc.updateDeploymentAndClearRollbackTo(ctx, d)
}

func (dc *DeploymentController) emitRollbackWarningEvent(d *apps.Deployment, reason, message string) {
	dc.eventRecorder.Eventf(d, v1.EventTypeWarning, reason, message)
}

func (dc *DeploymentController) emitRollbackNormalEvent(d *apps.Deployment, message string) {
	dc.eventRecorder.Eventf(d, v1.EventTypeNormal, deploymentutil.RollbackDone, message)
}

// updateDeploymentAndClearRollbackTo sets .spec.rollbackTo to nil and update the input deployment
// It is assumed that the caller will have updated the deployment template appropriately (in case
// we want to rollback).
func (dc *DeploymentController) updateDeploymentAndClearRollbackTo(ctx context.Context, d *apps.Deployment) error {
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Cleans up rollbackTo of deployment", "deployment", klog.KObj(d))
	setRollbackTo(d, nil)
	_, err := dc.client.AppsV1().Deployments(d.Namespace).Update(ctx, d, metav1.UpdateOptions{})
	return err
}

// TODO: Remove this when extensions/v1beta1 and apps/v1beta1 Deployment are dropped.
// getRollbackTo 通过判断 deployment 是否存在 rollback 对应的注解然后获取其值作为目标版本。
func getRollbackTo(d *apps.Deployment) *extensions.RollbackConfig {
	// Extract the annotation used for round-tripping the deprecated RollbackTo field.
	revision := d.Annotations[apps.DeprecatedRollbackTo]
	if revision == "" {
		return nil
	}
	revision64, err := strconv.ParseInt(revision, 10, 64)
	if err != nil {
		// If it's invalid, ignore it.
		return nil
	}
	return &extensions.RollbackConfig{
		Revision: revision64,
	}
}

// TODO: Remove this when extensions/v1beta1 and apps/v1beta1 Deployment are dropped.
func setRollbackTo(d *apps.Deployment, rollbackTo *extensions.RollbackConfig) {
	if rollbackTo == nil {
		delete(d.Annotations, apps.DeprecatedRollbackTo)
		return
	}
	if d.Annotations == nil {
		d.Annotations = make(map[string]string)
	}
	d.Annotations[apps.DeprecatedRollbackTo] = strconv.FormatInt(rollbackTo.Revision, 10)
}
