package main

import (
	"context"
	"errors"
)

type Result struct {
	Target AuditTarget
	Data   SEOData
	Error  error
}

type ResultSummary struct {
	Received            int
	Saved               int
	Successful          int
	Failed              int
	PersistenceFailures int
}

func failedScanResult(data SEOData, code string, err error) Result {
	data.ScanStatus = scanStatusFailed
	data.ErrorCode = code
	data.ErrorMessage = sanitizeError(err)
	return Result{Data: data, Error: errors.New(data.ErrorMessage)}
}

type targetURLRecord struct {
	ID  int64
	URL string
}

type AuditTarget struct {
	TargetID    int64
	RequestURL  string
	SafeURL     string
	Fingerprint []byte
}

type targetURLSnapshot struct {
	HighWatermark int64
	Total         int64
	Successful    int64
	Failed        int64
}

type urlStreamSummary struct {
	Queued  int
	Skipped int
	Error   error
}

type targetURLBatchClaimer func(ctx context.Context, limit int) ([]targetURLRecord, error)
