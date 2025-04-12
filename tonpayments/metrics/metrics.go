package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	ChannelBalance = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "channels_balance",
			Namespace: "payments",
			Help:      "The current balance of channels.",
		},
		[]string{"peer", "coin", "is_our", "balance_type"},
	)

	ActiveVirtualChannels = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "virtual_channels",
			Namespace: "payments",
			Help:      "Active virtual channels count.",
		},
		[]string{"peer", "coin", "is_out", "want_remove"},
	)

	ActiveVirtualChannelsCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "virtual_channels_capacity",
			Namespace: "payments",
			Help:      "Active virtual channels capacity.",
		},
		[]string{"peer", "coin", "is_out", "want_remove"},
	)

	ActiveVirtualChannelsFee = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "virtual_channels_fee",
			Namespace: "payments",
			Help:      "Active virtual channels fee.",
		},
		[]string{"peer", "coin", "is_out", "want_remove"},
	)

	QueuedTasks = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "queued_tasks",
			Namespace: "payments",
			Help:      "Number of tasks in the queue.",
		},
		[]string{"job_type", "in_retry", "execute_later"},
	)
)

var Registered = false

func RegisterMetrics() {
	if Registered {
		return
	}
	Registered = true

	prometheus.MustRegister(ChannelBalance)
	prometheus.MustRegister(ActiveVirtualChannels)
	prometheus.MustRegister(QueuedTasks)
	prometheus.MustRegister(ActiveVirtualChannelsCapacity)
	prometheus.MustRegister(ActiveVirtualChannelsFee)
}
