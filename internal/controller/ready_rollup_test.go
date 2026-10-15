/*
Copyright 2026.

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

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("upgradeHasFailed (Ready rollup)", func() {
	DescribeTable("which UpgradeReady conditions take the store out of Ready",
		func(status metav1.ConditionStatus, reason string, want bool) {
			c := metav1.Condition{Type: ConditionUpgradeReady, Status: status, Reason: reason}
			Expect(upgradeHasFailed(c)).To(Equal(want))
		},
		// A failed setup:upgrade Job reports the Job-recovery Reasons;
		// before this check matched them, a fenced store with a failed
		// upgrade still reported Ready=True.
		Entry("retrying a failed Job", metav1.ConditionFalse, JobRecoveryRetryReason, true),
		Entry("retry budget exhausted", metav1.ConditionFalse, JobRecoveryReason, true),
		Entry("cron quiesce timeout", metav1.ConditionFalse, ReasonCronQuiesceTimeout, true),
		Entry("arc in flight", metav1.ConditionFalse, ReasonWorkloadRolloutPending, false),
		Entry("arc starting", metav1.ConditionFalse, ReasonUpgradeStarting, false),
		Entry("up to date", metav1.ConditionTrue, "UpToDate", false),
		Entry("fresh install", metav1.ConditionUnknown, ReasonAwaitingUpgrade, false),
	)
})
