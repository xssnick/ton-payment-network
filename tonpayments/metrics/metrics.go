package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	WalletBalance                 *prometheus.GaugeVec
	ChannelBalance                *prometheus.GaugeVec
	ActiveVirtualChannels         *prometheus.GaugeVec
	ActiveVirtualChannelsCapacity *prometheus.GaugeVec
	ActiveVirtualChannelsFee      *prometheus.GaugeVec
	QueuedTasks                   *prometheus.GaugeVec
)

var Registered = false

func RegisterMetrics(namespace string) {
	if Registered {
		return
	}
	Registered = true

	WalletBalance = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "wallet_balance",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "The current balance of wallet.",
		},
		[]string{"coin"},
	)

	ChannelBalance = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "channels_balance",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "The current balance of channels.",
		},
		[]string{"peer", "coin", "is_our", "balance_type"},
	)

	ActiveVirtualChannels = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "virtual_channels",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "Active virtual channels count.",
		},
		[]string{"peer", "coin", "is_out", "want_remove"},
	)

	ActiveVirtualChannelsCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "virtual_channels_capacity",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "Active virtual channels capacity.",
		},
		[]string{"peer", "coin", "is_out", "want_remove"},
	)

	ActiveVirtualChannelsFee = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "virtual_channels_fee",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "Active virtual channels fee.",
		},
		[]string{"peer", "coin", "is_out", "want_remove"},
	)

	QueuedTasks = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "queued_tasks",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "Number of tasks in the queue.",
		},
		[]string{"job_type", "in_retry", "execute_later"},
	)

	prometheus.MustRegister(ChannelBalance)
	prometheus.MustRegister(ActiveVirtualChannels)
	prometheus.MustRegister(QueuedTasks)
	prometheus.MustRegister(ActiveVirtualChannelsCapacity)
	prometheus.MustRegister(ActiveVirtualChannelsFee)
	prometheus.MustRegister(WalletBalance)
}
