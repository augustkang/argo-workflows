package cron

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned/fake"
	"github.com/argoproj/argo-workflows/v3/util/humanize"
	"github.com/argoproj/argo-workflows/v3/util/logging"
	"github.com/argoproj/argo-workflows/v3/util/telemetry"
	"github.com/argoproj/argo-workflows/v3/workflow/common"
	"github.com/argoproj/argo-workflows/v3/workflow/metrics"
	"github.com/argoproj/argo-workflows/v3/workflow/util"
)

var scheduledWf = `
  apiVersion: argoproj.io/v1alpha1
  kind: CronWorkflow
  metadata:
    creationTimestamp: "2020-02-28T18:31:32Z"
    generation: 69
    name: hello-world
    namespace: argo
    resourceVersion: "53389"
    selfLink: /apis/argoproj.io/v1alpha1/namespaces/argo/cronworkflows/hello-world
    uid: f230ee83-2ddc-435e-b27c-f0ca63293100
  spec:
    schedules:
      - '* * * * *'
    startingDeadlineSeconds: 30
    workflowSpec:
      entrypoint: whalesay
      templates:
      - container:
          args:
          - "\U0001F553 hello world"
          command:
          - cowsay
          image: docker/whalesay:latest
          name: ""
          resources: {}
        inputs: {}
        metadata: {}
        name: whalesay
        outputs: {}
  status:
    lastScheduledTime: "2020-02-28T19:05:00Z"
`

func TestRunOutstandingWorkflows(t *testing.T) {
	// To ensure consistency, always start at the next 30 second mark
	_, _, sec := time.Now().Clock()
	ctx := logging.TestContext(t.Context())
	var toWait time.Duration
	if sec <= 30 {
		toWait = time.Duration(30-sec) * time.Second
	} else {
		toWait = time.Duration(90-sec) * time.Second
	}
	t.Logf("Waiting %s to start", humanize.Duration(toWait))
	time.Sleep(toWait)

	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(scheduledWf), &cronWf)

	// Second value at runtime should be 30-31

	cronWf.Status.LastScheduledTime = &v1.Time{Time: time.Now().Add(-1 * time.Minute)}
	// StartingDeadlineSeconds is after the current second, so cron should be run
	cronWf.Spec.StartingDeadlineSeconds = ptr.To(int64(35))
	woc := &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	woc.cronWf.SetSchedule(woc.cronWf.Spec.GetScheduleWithTimezoneString())
	missedExecutionTime, err := woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	// The missedExecutionTime should be the last complete minute mark, which we can get with inferScheduledTime
	assert.Equal(t, inferScheduledTime(ctx).Unix(), missedExecutionTime.Unix())

	// StartingDeadlineSeconds is not after the current second, so cron should not be run
	cronWf.Spec.StartingDeadlineSeconds = ptr.To(int64(25))
	woc = &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	assert.True(t, missedExecutionTime.IsZero())

	// Same test, but simulate a change to the schedule immediately prior by setting a different last-used-schedule annotation
	// In this case, since a schedule change is detected, not workflow should be run
	woc.cronWf.SetSchedule("0 * * * *")
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	assert.True(t, missedExecutionTime.IsZero())

	// Run the same test in a different timezone
	testTimezone := "Pacific/Niue"
	testLocation, err := time.LoadLocation(testTimezone)
	if err != nil {
		panic(err)
	}
	cronWf.Spec.Timezone = testTimezone
	cronWf.Status.LastScheduledTime = &v1.Time{Time: cronWf.Status.LastScheduledTime.In(testLocation)}

	// StartingDeadlineSeconds is after the current second, so cron should be run
	cronWf.Spec.StartingDeadlineSeconds = ptr.To(int64(35))
	woc = &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	// Reset last-used-schedule as if the current schedule has been used before
	woc.cronWf.SetSchedule(woc.cronWf.Spec.GetScheduleWithTimezoneString())
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	// The missedExecutionTime should be the last complete minute mark, which we can get with inferScheduledTime
	assert.Equal(t, inferScheduledTime(ctx).Unix(), missedExecutionTime.Unix())

	// StartingDeadlineSeconds is not after the current second, so cron should not be run
	cronWf.Spec.StartingDeadlineSeconds = ptr.To(int64(25))
	woc = &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	assert.True(t, missedExecutionTime.IsZero())

	// Same test, but simulate a change to the schedule immediately prior by setting a different last-used-schedule annotation
	// In this case, since a schedule change is detected, not workflow should be run
	woc.cronWf.SetSchedule("0 * * * *")
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	assert.True(t, missedExecutionTime.IsZero())
}

func getCWFShouldJustHaveStarted(locationStr string, loc *time.Location) v1alpha1.CronWorkflow {
	oneMinuteAgo := time.Now().Add(-1 * time.Minute).In(loc)
	cwf := fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: CronWorkflow
metadata:
  name: start
spec:
  schedules:
    - "%d %d * * *"
  timezone: "%s"
  startingDeadlineSeconds: 120
  workflowSpec:
    entrypoint: whalesay
    templates:
      - name: whalesay
        container:
          image: argoproj/argosay:v2
status:
  lastScheduledTime: "2020-02-28T19:05:00Z"`,
		oneMinuteAgo.Minute(),
		oneMinuteAgo.Hour(),
		locationStr,
	)
	fmt.Printf("%s\n", cwf)
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(cwf), &cronWf)
	return cronWf
}

func TestRunOutstandingWorkflowsAcrossTimezones(t *testing.T) {
	// To ensure consistency, always start at the next 30 second mark
	_, _, sec := time.Now().Clock()
	ctx := logging.TestContext(t.Context())
	var toWait time.Duration
	if sec <= 30 {
		toWait = time.Duration(30-sec) * time.Second
	} else {
		toWait = time.Duration(90-sec) * time.Second
	}
	t.Logf("Waiting %s to start", humanize.Duration(toWait))
	time.Sleep(toWait)

	const testLocation = "Pacific/Auckland"
	locAuckland, err := time.LoadLocation(testLocation)
	require.NoError(t, err)
	cronWf := getCWFShouldJustHaveStarted(testLocation, locAuckland)
	// Second value at runtime should be 30-31

	cronWf.Status.LastScheduledTime = &v1.Time{Time: time.Now().Add(-24*time.Hour + -1*time.Minute)}
	woc := &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	woc.cronWf.SetSchedule(woc.cronWf.Spec.GetScheduleWithTimezoneString())
	missedExecutionTime, err := woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	// The missedExecutionTime should be the current complete minute mark, which we can get with inferScheduledTime
	assert.Equal(t, inferScheduledTime(ctx).Unix(), missedExecutionTime.Unix()+60)

	// We are assuming local time is not Auckland here
	locHere := time.Now().Local().Location()
	assert.NotEqual(t, locHere, locAuckland, "If you are in New Zealand and this test fails you'll need to modify the test, it's not a real failure")
	cronWf = getCWFShouldJustHaveStarted(testLocation, locHere)
	// Second value at runtime should be 30-31

	cronWf.Status.LastScheduledTime = &v1.Time{Time: time.Now().Add(-24*time.Hour + -1*time.Minute)}
	woc = &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	woc.cronWf.SetSchedule(woc.cronWf.Spec.GetScheduleWithTimezoneString())
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	// We're outside the window for execution now
	assert.True(t, missedExecutionTime.IsZero())
}

type fakeLister struct{}

func (f fakeLister) List() ([]*v1alpha1.Workflow, error) {
	// Do nothing
	return nil, nil
}

var _ util.WorkflowLister = &fakeLister{}

var invalidWf = `
  apiVersion: argoproj.io/v1alpha1
  kind: CronWorkflow
  metadata:
    name: hello-world
  spec:
    schedules:
      - '* * * * *'
    startingDeadlineSeconds: 30
    workflowSpec:
      entrypoint: whalesay
      templates:
      - container:
          args:
          - "\U0001F553 hello world"
          command:
          - cowsay
          image: docker/whalesay:latest
          name: ""
          resources: {}
        inputs: {}
        metadata: {}
        name: "bad template name"
        outputs: {}
`

func TestCronWorkflowConditionSubmissionError(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(invalidWf), &cronWf)

	cs := fake.NewSimpleClientset()
	testMetrics, err := metrics.New(logging.TestContext(t.Context()), telemetry.TestScopeName, telemetry.TestScopeName, &telemetry.Config{}, metrics.Callbacks{})
	require.NoError(t, err)
	woc := &cronWfOperationCtx{
		wfClientset:       cs,
		wfClient:          cs.ArgoprojV1alpha1().Workflows(""),
		cronWfIf:          cs.ArgoprojV1alpha1().CronWorkflows(""),
		cronWf:            &cronWf,
		log:               logging.RequireLoggerFromContext(ctx),
		metrics:           testMetrics,
		scheduledTimeFunc: inferScheduledTime,
		ctx:               ctx,
	}
	woc.Run()

	assert.Len(t, woc.cronWf.Status.Conditions, 1)
	submissionErrorCond := woc.cronWf.Status.Conditions[0]
	assert.Equal(t, v1.ConditionTrue, submissionErrorCond.Status)
	assert.Equal(t, v1alpha1.ConditionTypeSpecError, submissionErrorCond.Type)
	assert.Contains(t, submissionErrorCond.Message, "'bad template name' is invalid")
}

var specError = `
apiVersion: argoproj.io/v1alpha1
kind: CronWorkflow
metadata:
  name: hello-world
spec:
  concurrencyPolicy: Replace
  failedJobsHistoryLimit: 4
  schedules:
    - 10 * * 12737123 *
  startingDeadlineSeconds: 0
  successfulJobsHistoryLimit: 4
  timezone: America/Los_Angeles
  workflowSpec:
    entrypoint: whalesay
    templates:
    -
      container:
        args:
        - "\U0001F553 hello world"
        command:
        - cowsay
        image: docker/whalesay:latest
        name: ""
        resources: {}
      inputs: {}
      metadata: {}
      name: whalesay
      outputs: {}
`

func TestSpecError(t *testing.T) {
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(specError), &cronWf)

	cs := fake.NewSimpleClientset()
	ctx := logging.TestContext(t.Context())
	testMetrics, err := metrics.New(ctx, telemetry.TestScopeName, telemetry.TestScopeName, &telemetry.Config{}, metrics.Callbacks{})
	require.NoError(t, err)
	woc := &cronWfOperationCtx{
		wfClientset: cs,
		wfClient:    cs.ArgoprojV1alpha1().Workflows(""),
		cronWfIf:    cs.ArgoprojV1alpha1().CronWorkflows(""),
		cronWf:      &cronWf,
		log:         logging.RequireLoggerFromContext(ctx),
		metrics:     testMetrics,
	}

	err = woc.validateCronWorkflow(ctx)
	require.Error(t, err)
	assert.Len(t, woc.cronWf.Status.Conditions, 1)
	submissionErrorCond := woc.cronWf.Status.Conditions[0]
	assert.Equal(t, v1.ConditionTrue, submissionErrorCond.Status)
	assert.Equal(t, v1alpha1.ConditionTypeSpecError, submissionErrorCond.Type)
	assert.Contains(t, submissionErrorCond.Message, "cron schedule 10 * * 12737123 * is malformed: end of range (12737123) above maximum (12): 12737123")
}

func TestScheduleTimeParam(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(scheduledWf), &cronWf)

	cs := fake.NewSimpleClientset()
	testMetrics, _ := metrics.New(ctx, telemetry.TestScopeName, telemetry.TestScopeName, &telemetry.Config{}, metrics.Callbacks{})
	woc := &cronWfOperationCtx{
		wfClientset:       cs,
		wfClient:          cs.ArgoprojV1alpha1().Workflows(""),
		cronWfIf:          cs.ArgoprojV1alpha1().CronWorkflows(""),
		cronWf:            &cronWf,
		log:               logging.RequireLoggerFromContext(ctx),
		metrics:           testMetrics,
		scheduledTimeFunc: inferScheduledTime,
		ctx:               ctx,
	}
	woc.Run()
	wsl, err := cs.ArgoprojV1alpha1().Workflows("").List(ctx, v1.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, wsl.Items.Len())
	wf := wsl.Items[0]
	assert.NotNil(t, wf)
	assert.Len(t, wf.GetAnnotations(), 1)
	assert.NotEmpty(t, wf.GetAnnotations()[common.AnnotationKeyCronWfScheduledTime])
}

const lastUsedSchedule = `apiVersion: argoproj.io/v1alpha1
kind: CronWorkflow
metadata:
  name: test
spec:
  concurrencyPolicy: Forbid
  failedJobsHistoryLimit: 1
  schedules:
    - 41 12 * * *
  successfulJobsHistoryLimit: 1
  timezone: America/New_York
  workflowSpec:
    arguments: {}
    entrypoint: job
    templates:
    - container:
        args:
        - /bin/echo "hello argo"
        command:
        - /bin/sh
        - -c
        image: alpine
        imagePullPolicy: Always
      name: job
`

func TestLastUsedSchedule(t *testing.T) {
	var cronWf v1alpha1.CronWorkflow
	ctx := logging.TestContext(t.Context())
	v1alpha1.MustUnmarshal([]byte(lastUsedSchedule), &cronWf)

	cs := fake.NewSimpleClientset()
	testMetrics, err := metrics.New(ctx, telemetry.TestScopeName, telemetry.TestScopeName, &telemetry.Config{}, metrics.Callbacks{})
	require.NoError(t, err)
	woc := &cronWfOperationCtx{
		wfClientset:       cs,
		wfClient:          cs.ArgoprojV1alpha1().Workflows(""),
		cronWfIf:          cs.ArgoprojV1alpha1().CronWorkflows(""),
		cronWf:            &cronWf,
		log:               logging.RequireLoggerFromContext(ctx),
		metrics:           testMetrics,
		scheduledTimeFunc: inferScheduledTime,
	}

	missedExecutionTime, err := woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	assert.Equal(t, time.Time{}, missedExecutionTime)

	woc.cronWf.SetSchedule(woc.cronWf.Spec.GetScheduleWithTimezoneString())

	require.NotNil(t, woc.cronWf.Annotations)
	assert.Equal(t, woc.cronWf.Spec.GetScheduleWithTimezoneString(), woc.cronWf.GetLatestSchedule())
}

var forbidMissedSchedule = `apiVersion: argoproj.io/v1alpha1
kind: CronWorkflow
metadata:
  annotations:
    cronworkflows.argoproj.io/last-used-schedule: CRON_TZ=America/Los_Angeles 0-36/1
      21-22 * * *
  creationTimestamp: "2022-02-04T05:33:24Z"
  generation: 2
  name: hello-world
  namespace: argo
  resourceVersion: "341102"
  uid: 9ac888d8-95e3-4f93-8983-0d46c6c7d62a
spec:
  concurrencyPolicy: Forbid
  failedJobsHistoryLimit: 4
  schedules:
    - 0-36/1 21-22 * * *
  startingDeadlineSeconds: 0
  successfulJobsHistoryLimit: 4
  timezone: America/Los_Angeles
  workflowSpec:
    arguments: {}
    entrypoint: whalesay
    templates:
    - container:
        args:
        - sleep 600
        command:
        - sh
        - -c
        image: alpine:3.6
        name: ""
        resources: {}
      inputs: {}
      metadata: {}
      name: whalesay
      outputs: {}
status:
  active:
  - apiVersion: argoproj.io/v1alpha1
    kind: Workflow
    name: hello-world-1643952840
    namespace: argo
    resourceVersion: "341101"
    uid: c56a8f98-ff46-4815-9d6f-d9db5cfcd941
  lastScheduledTime: "2022-02-04T05:34:00Z"
`

func TestMissedScheduleAfterCronScheduleWithForbid(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(forbidMissedSchedule), &cronWf)
	// StartingDeadlineSeconds is after the current second, so cron should be run
	//startingDeadlineSeconds := int64(35)
	//cronWf.Spec.StartingDeadlineSeconds = &startingDeadlineSeconds
	t.Run("ForbiddenWithMissedScheduleAfterCron", func(t *testing.T) {
		cronWf.Spec.StartingDeadlineSeconds = nil
		woc := &cronWfOperationCtx{
			cronWf: &cronWf,
			log:    logging.RequireLoggerFromContext(ctx),
		}
		woc.cronWf.SetSchedule(woc.cronWf.Spec.GetScheduleWithTimezoneString())
		missedExecutionTime, err := woc.shouldOutstandingWorkflowsBeRun(ctx)
		require.NoError(t, err)
		assert.True(t, missedExecutionTime.IsZero())
	})
}

var multipleSchedulesWf = `
  apiVersion: argoproj.io/v1alpha1
  kind: CronWorkflow
  metadata:
    creationTimestamp: "2020-02-28T18:31:32Z"
    generation: 69
    name: hello-world
    namespace: argo
    resourceVersion: "53389"
    selfLink: /apis/argoproj.io/v1alpha1/namespaces/argo/cronworkflows/hello-world
    uid: f230ee83-2ddc-435e-b27c-f0ca63293100
  spec:
    schedules:
    - "* * * * *"
    - "0 * * * *"
    startingDeadlineSeconds: 30
    workflowSpec:
      entrypoint: whalesay
      templates:
      - container:
          args:
          - "\U0001F553 hello world"
          command:
          - cowsay
          image: docker/whalesay:latest
          name: ""
          resources: {}
        inputs: {}
        metadata: {}
        name: whalesay
        outputs: {}
  status:
    lastScheduledTime: "2020-02-28T19:05:00Z"
`

func TestMultipleSchedules(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(multipleSchedulesWf), &cronWf)

	cs := fake.NewSimpleClientset()
	testMetrics, err := metrics.New(ctx, telemetry.TestScopeName, telemetry.TestScopeName, &telemetry.Config{}, metrics.Callbacks{})
	require.NoError(t, err)
	woc := &cronWfOperationCtx{
		wfClientset:       cs,
		wfClient:          cs.ArgoprojV1alpha1().Workflows(""),
		cronWfIf:          cs.ArgoprojV1alpha1().CronWorkflows(""),
		cronWf:            &cronWf,
		log:               logging.RequireLoggerFromContext(ctx),
		metrics:           testMetrics,
		scheduledTimeFunc: inferScheduledTime,
		ctx:               ctx,
	}
	woc.Run()
	wsl, err := cs.ArgoprojV1alpha1().Workflows("").List(ctx, v1.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, wsl.Items.Len())
	wf := wsl.Items[0]
	assert.NotNil(t, wf)
	assert.Len(t, wf.GetAnnotations(), 1)
	assert.NotEmpty(t, wf.GetAnnotations()[common.AnnotationKeyCronWfScheduledTime])
}

var specErrWithScheduleAndSchedules = `
  apiVersion: argoproj.io/v1alpha1
  kind: CronWorkflow
  metadata:
    creationTimestamp: "2020-02-28T18:31:32Z"
    generation: 69
    name: hello-world
    namespace: argo
    resourceVersion: "53389"
    selfLink: /apis/argoproj.io/v1alpha1/namespaces/argo/cronworkflows/hello-world
    uid: f230ee83-2ddc-435e-b27c-f0ca63293100
  spec:
    schedule: "* * * * *"
    schedules:
    - "* * * * *"
    - "0 * * * *"
    startingDeadlineSeconds: 30
    workflowSpec:
      entrypoint: whalesay
      templates:
      - container:
          args:
          - "\U0001F553 hello world"
          command:
          - cowsay
          image: docker/whalesay:latest
          name: ""
          resources: {}
        inputs: {}
        metadata: {}
        name: whalesay
        outputs: {}
  status:
    lastScheduledTime: "2020-02-28T19:05:00Z"
`

func TestSpecErrorWithScheduleAndSchedules(t *testing.T) {
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(specErrWithScheduleAndSchedules), &cronWf)

	cs := fake.NewSimpleClientset()
	ctx := logging.TestContext(t.Context())
	testMetrics, err := metrics.New(ctx, telemetry.TestScopeName, telemetry.TestScopeName, &telemetry.Config{}, metrics.Callbacks{})
	require.NoError(t, err)
	woc := &cronWfOperationCtx{
		wfClientset: cs,
		wfClient:    cs.ArgoprojV1alpha1().Workflows(""),
		cronWfIf:    cs.ArgoprojV1alpha1().CronWorkflows(""),
		cronWf:      &cronWf,
		log:         logging.RequireLoggerFromContext(ctx),
		metrics:     testMetrics,
	}

	err = woc.validateCronWorkflow(ctx)
	require.Error(t, err)
	assert.Len(t, woc.cronWf.Status.Conditions, 1)
	submissionErrorCond := woc.cronWf.Status.Conditions[0]
	assert.Equal(t, v1.ConditionTrue, submissionErrorCond.Status)
	assert.Equal(t, v1alpha1.ConditionTypeSpecError, submissionErrorCond.Type)
	assert.Contains(t, submissionErrorCond.Message, "cron workflow cant be configured with both Spec.Schedule and Spec.Schedules")
}

var specErrWithValidAndInvalidSchedules = `
  apiVersion: argoproj.io/v1alpha1
  kind: CronWorkflow
  metadata:
    creationTimestamp: "2020-02-28T18:31:32Z"
    generation: 69
    name: hello-world
    namespace: argo
    resourceVersion: "53389"
    selfLink: /apis/argoproj.io/v1alpha1/namespaces/argo/cronworkflows/hello-world
    uid: f230ee83-2ddc-435e-b27c-f0ca63293100
  spec:
    schedules:
    - "* * * * *"
    - "10 * * 12737123 *"
    startingDeadlineSeconds: 30
    workflowSpec:
      entrypoint: whalesay
      templates:
      - container:
          args:
          - "\U0001F553 hello world"
          command:
          - cowsay
          image: docker/whalesay:latest
          name: ""
          resources: {}
        inputs: {}
        metadata: {}
        name: whalesay
        outputs: {}
  status:
    lastScheduledTime: "2020-02-28T19:05:00Z"
`

func TestSpecErrorWithValidAndInvalidSchedules(t *testing.T) {
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(specErrWithValidAndInvalidSchedules), &cronWf)

	cs := fake.NewSimpleClientset()
	ctx := logging.TestContext(t.Context())
	testMetrics, err := metrics.New(ctx, telemetry.TestScopeName, telemetry.TestScopeName, &telemetry.Config{}, metrics.Callbacks{})
	require.NoError(t, err)
	woc := &cronWfOperationCtx{
		wfClientset: cs,
		wfClient:    cs.ArgoprojV1alpha1().Workflows(""),
		cronWfIf:    cs.ArgoprojV1alpha1().CronWorkflows(""),
		cronWf:      &cronWf,
		log:         logging.RequireLoggerFromContext(ctx),
		metrics:     testMetrics,
	}

	err = woc.validateCronWorkflow(ctx)
	require.Error(t, err)
	assert.Len(t, woc.cronWf.Status.Conditions, 1)
	submissionErrorCond := woc.cronWf.Status.Conditions[0]
	assert.Equal(t, v1.ConditionTrue, submissionErrorCond.Status)
	assert.Equal(t, v1alpha1.ConditionTypeSpecError, submissionErrorCond.Type)
	assert.Contains(t, submissionErrorCond.Message, "cron schedule 10 * * 12737123 * is malformed: end of range (12737123) above maximum (12): 12737123")
}

// TestRunOutstandingWorkflows is the same test as TestRunOutstandingWorkflows but using multiple schedules configured
func TestRunOutstandingWorkflowsWithMultipleSchedules(t *testing.T) {
	// To ensure consistency, always start at the next 30 second mark
	_, _, sec := time.Now().Clock()
	ctx := logging.TestContext(t.Context())
	var toWait time.Duration
	if sec <= 30 {
		toWait = time.Duration(30-sec) * time.Second
	} else {
		toWait = time.Duration(90-sec) * time.Second
	}
	t.Logf("Waiting %s to start", humanize.Duration(toWait))
	time.Sleep(toWait)

	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(multipleSchedulesWf), &cronWf)

	// Second value at runtime should be 30-31

	cronWf.Status.LastScheduledTime = &v1.Time{Time: time.Now().Add(-1 * time.Minute)}
	// StartingDeadlineSeconds is after the current second, so cron should be run
	startingDeadlineSeconds := int64(35)
	cronWf.Spec.StartingDeadlineSeconds = &startingDeadlineSeconds
	woc := &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	woc.cronWf.SetSchedule(woc.cronWf.Spec.GetScheduleWithTimezoneString())
	missedExecutionTime, err := woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	// The missedExecutionTime should be the last complete minute mark, which we can get with inferScheduledTime
	assert.Equal(t, inferScheduledTime(ctx).Unix(), missedExecutionTime.Unix())

	// StartingDeadlineSeconds is not after the current second, so cron should not be run
	startingDeadlineSeconds = int64(25)
	cronWf.Spec.StartingDeadlineSeconds = &startingDeadlineSeconds
	woc = &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	assert.True(t, missedExecutionTime.IsZero())

	// Same test, but simulate a change to the schedule immediately prior by setting a different last-used-schedule annotation
	// In this case, since a schedule change is detected, not workflow should be run
	woc.cronWf.SetSchedules([]string{"0 * * * *,1 * * * *"})
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	assert.True(t, missedExecutionTime.IsZero())

	// Run the same test in a different timezone
	testTimezone := "Pacific/Niue"
	testLocation, err := time.LoadLocation(testTimezone)
	if err != nil {
		panic(err)
	}
	cronWf.Spec.Timezone = testTimezone
	cronWf.Status.LastScheduledTime = &v1.Time{Time: cronWf.Status.LastScheduledTime.In(testLocation)}

	// StartingDeadlineSeconds is after the current second, so cron should be run
	startingDeadlineSeconds = int64(35)
	cronWf.Spec.StartingDeadlineSeconds = &startingDeadlineSeconds
	woc = &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	// Reset last-used-schedule as if the current schedule has been used before
	woc.cronWf.SetSchedule(woc.cronWf.Spec.GetScheduleWithTimezoneString())
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	// The missedExecutionTime should be the last complete minute mark, which we can get with inferScheduledTime
	assert.Equal(t, inferScheduledTime(ctx).Unix(), missedExecutionTime.Unix())

	// StartingDeadlineSeconds is not after the current second, so cron should not be run
	startingDeadlineSeconds = int64(25)
	cronWf.Spec.StartingDeadlineSeconds = &startingDeadlineSeconds
	woc = &cronWfOperationCtx{
		cronWf: &cronWf,
		log:    logging.RequireLoggerFromContext(ctx),
	}
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	assert.True(t, missedExecutionTime.IsZero())

	// Same test, but simulate a change to the schedule immediately prior by setting a different last-used-schedule annotation
	// In this case, since a schedule change is detected, not workflow should be run
	woc.cronWf.SetSchedules([]string{"0 * * * *,1 * * * *"})
	missedExecutionTime, err = woc.shouldOutstandingWorkflowsBeRun(ctx)
	require.NoError(t, err)
	assert.True(t, missedExecutionTime.IsZero())
}

func TestEvaluateWhen(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(scheduledWf), &cronWf)

	cronWf.Spec.When = "{{= cronworkflow.lastScheduledTime == nil || ( (now() - cronworkflow.lastScheduledTime).Seconds() > 30) }}"
	result, err := evalWhen(ctx, &cronWf)
	require.NoError(t, err)
	assert.True(t, result)

	cronWf.Spec.When = "{{= cronworkflow.lastScheduledTime == nil && ( (now() - cronworkflow.lastScheduledTime).Seconds() < 30) }}"
	result, err = evalWhen(ctx, &cronWf)
	require.NoError(t, err)
	assert.False(t, result)

	cronWf.Spec.When = "{{= cronworkflow.lastScheduledTime != nil }}"
	result, err = evalWhen(ctx, &cronWf)
	require.NoError(t, err)
	assert.True(t, result)

	cronWf.Status.LastScheduledTime = nil
	cronWf.Spec.When = "{{= cronworkflow.lastScheduledTime == nil }}"
	result, err = evalWhen(ctx, &cronWf)
	require.NoError(t, err)
	assert.True(t, result)

	cronWf.Status.LastScheduledTime = &v1.Time{Time: time.Now().Add(time.Minute * -30)}
	cronWf.Spec.When = "{{= (now() - cronworkflow.lastScheduledTime).Minutes() >= 30 }}"
	result, err = evalWhen(ctx, &cronWf)
	require.NoError(t, err)
	assert.True(t, result)

	cronWf.Spec.When = "{{= (now() - cronworkflow.lastScheduledTime).Minutes() <  50 }}"
	result, err = evalWhen(ctx, &cronWf)
	require.NoError(t, err)
	assert.True(t, result)
}

func TestEvaluateWhenUnresolvedOutside(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	var cronWf v1alpha1.CronWorkflow
	v1alpha1.MustUnmarshal([]byte(scheduledWf), &cronWf)
	param := v1alpha1.Parameter{Name: "scheduled-time", Value: v1alpha1.AnyStringPtr("{{workflow.scheduledTime}}")}
	params := []v1alpha1.Parameter{param}
	argument := v1alpha1.Arguments{Parameters: params}
	cronWf.Spec.WorkflowSpec.Arguments = argument

	cronWf.Status.LastScheduledTime = &v1.Time{Time: time.Now().Add(time.Minute * -30)}
	cronWf.Spec.When = "{{= (now() - cronworkflow.lastScheduledTime).Minutes() >= 30 }}"
	result, err := evalWhen(ctx, &cronWf)
	require.NoError(t, err)
	assert.True(t, result)
}
