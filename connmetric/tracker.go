package connmetric

import (
	"time"
)

type StatsEntry struct {
	Address  string        `json:"address"`
	Duration time.Duration `json:"duration"`
	BytesIn  uint64        `json:"bytes_in"`
	BytesOut uint64        `json:"bytes_out"`
	Error    error         `json:"error"`
}

type Tracker interface {
	RecordDial(string, bool)
	RecordStats(StatsEntry)
}
