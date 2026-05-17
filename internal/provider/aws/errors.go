package aws

import (
	"errors"
	"strings"

	"github.com/aws/smithy-go"
)

// isAlreadyExists matches the family of AWS errors that signal "the thing you
// were trying to create is already there". We use it to keep create-or-attach
// paths idempotent without an extra Describe round-trip.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "InvalidPermission.Duplicate",
			"RouteAlreadyExists",
			"Resource.AlreadyAssociated",
			"EntityAlreadyExists",
			"InvalidGroup.Duplicate",
			"InvalidIPAddress.InUse":
			return true
		}
	}
	// Fallback: some EC2 errors only show up as "already" in the message.
	return strings.Contains(strings.ToLower(err.Error()), "already exists") ||
		strings.Contains(strings.ToLower(err.Error()), "already associated")
}
