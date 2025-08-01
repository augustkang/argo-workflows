package metrics

import (
	"context"

	"github.com/argoproj/argo-workflows/v3/util/telemetry"

	"go.opentelemetry.io/otel/metric"
)

// PodPhaseCallback is the function prototype to provide this gauge with the phase of the pods
type PodPhaseCallback func(ctx context.Context) map[string]int64

type podPhaseGauge struct {
	callback PodPhaseCallback
	gauge    *telemetry.Instrument
}

func addPodPhaseGauge(ctx context.Context, m *Metrics) error {
	err := m.CreateBuiltinInstrument(telemetry.InstrumentPodsGauge)
	if err != nil {
		return err
	}

	name := telemetry.InstrumentPodsGauge.Name()
	if m.callbacks.PodPhase != nil {
		ppGauge := podPhaseGauge{
			callback: m.callbacks.PodPhase,
			gauge:    m.GetInstrument(name),
		}
		return ppGauge.gauge.RegisterCallback(m.Metrics, ppGauge.update)
	}
	return nil
}

func (p *podPhaseGauge) update(ctx context.Context, o metric.Observer) error {
	phases := p.callback(ctx)
	for phase, val := range phases {
		p.gauge.ObserveInt(ctx, o, val, telemetry.InstAttribs{{Name: telemetry.AttribPodPhase, Value: phase}})
	}
	return nil
}
