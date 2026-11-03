// Package jobs names the queue job kinds shared by the API and workers.
package jobs

// Build pipeline job kinds.
const (
	Scan      = "build.scan"
	Test      = "build.test"
	Finalize  = "build.finalize"
	Publish   = "build.publish"
	Reproduce = "build.reproduce"
	Webhook   = "webhook.deliver"
)

// Payload is the JSON payload of build jobs.
func Payload(buildID string) []byte { return []byte(`{"buildId":"` + buildID + `"}`) }
