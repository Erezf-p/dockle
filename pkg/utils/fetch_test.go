package utils

import (
	"testing"

	"github.com/Portshift/dockle/pkg/log"
)

func TestFetchVersion(t *testing.T) {
	log.InitLogger(false)
	result, err := FetchLatestVersion()
	if err != nil {
		t.Errorf("fail to fetch version : %s", err)
	}
	if !versionPattern.MatchString(result) {
		t.Errorf("fail to fetch version : %s", result)
	}
}
