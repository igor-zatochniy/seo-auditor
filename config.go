package main

import appconfig "github.com/igor-zatochniy/seo-auditor/internal/config"

const (
	DefaultWorkers                   = appconfig.DefaultWorkers
	DefaultURLBatchSize              = appconfig.DefaultURLBatchSize
	DefaultShutdownTimeout           = appconfig.DefaultShutdownTimeout
	MaxURLBatchSize                  = appconfig.MaxURLBatchSize
	MaxWorkers                       = appconfig.MaxWorkers
	DefaultMaxHTMLBodyBytes          = appconfig.DefaultMaxHTMLBodyBytes
	MaxHTMLBodyBytes                 = appconfig.MaxHTMLBodyBytes
	DefaultMaxHTMLTokenBytes         = appconfig.DefaultMaxHTMLTokenBytes
	MaxHTMLTokenBytes                = appconfig.MaxHTMLTokenBytes
	MaxEstimatedHTMLParserHeapBytes  = appconfig.MaxEstimatedHTMLParserHeapBytes
	HTMLParserHeapAmplification      = appconfig.HTMLParserHeapAmplification
	DefaultHTTPMaxRetries            = appconfig.DefaultHTTPMaxRetries
	DefaultDBMaxRetries              = appconfig.DefaultDBMaxRetries
	MaxRetryAttempts                 = appconfig.MaxRetryAttempts
	DefaultRetryBaseDelay            = appconfig.DefaultRetryBaseDelay
	DefaultRetryMaxDelay             = appconfig.DefaultRetryMaxDelay
	DefaultHTTPAttemptTimeout        = appconfig.DefaultHTTPAttemptTimeout
	DefaultHTTPTotalTimeout          = appconfig.DefaultHTTPTotalTimeout
	DefaultRobotsAttemptTimeout      = appconfig.DefaultRobotsAttemptTimeout
	DefaultRobotsTotalTimeout        = appconfig.DefaultRobotsTotalTimeout
	DefaultDBConnectTimeout          = appconfig.DefaultDBConnectTimeout
	DefaultDBMigrationTimeout        = appconfig.DefaultDBMigrationTimeout
	DefaultDBFetchTimeout            = appconfig.DefaultDBFetchTimeout
	DefaultDBWriteTimeout            = appconfig.DefaultDBWriteTimeout
	DefaultReportExportTimeout       = appconfig.DefaultReportExportTimeout
	DefaultAuditRunHeartbeatInterval = appconfig.DefaultAuditRunHeartbeatInterval
	DefaultHeartbeatFailureThreshold = appconfig.DefaultHeartbeatFailureThreshold
	MaxHeartbeatFailureThreshold     = appconfig.MaxHeartbeatFailureThreshold
	DefaultStaleRunThreshold         = appconfig.DefaultStaleRunThreshold
	DefaultTargetLeaseDuration       = appconfig.DefaultTargetLeaseDuration
	DefaultTargetFingerprintKeyID    = appconfig.DefaultTargetFingerprintKeyID
	MinFingerprintKeyLen             = appconfig.MinFingerprintKeyLen
	MaxWorkerInstanceIDLen           = appconfig.MaxWorkerInstanceIDLen
)

type Config = appconfig.Config

var runIDPattern = appconfig.RunIDPattern

func loadConfig() (Config, error) {
	return appconfig.Load()
}
