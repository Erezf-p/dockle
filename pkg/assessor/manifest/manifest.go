package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	deckodertypes "github.com/goodwithtech/deckoder/types"

	"github.com/Portshift/dockle/pkg/log"

	"github.com/Portshift/dockle/pkg/types"
)

type ManifestAssessor struct{}

var ConfigFileName = "metadata"
var (
	sensitiveDirs    = map[string]struct{}{"/sys": {}, "/dev": {}, "/proc": {}}
	suspiciousEnvKey = []string{"PASSWD", "PASSWORD", "SECRET", "KEY", "ACCESS"}
	acceptanceEnvKey = map[string]struct{}{"GPG_KEY": {}, "GPG_KEYS": {}}
)

func (a ManifestAssessor) Assess(fileMap deckodertypes.FileMap) (assesses []*types.Assessment, err error) {
	log.Logger.Debug("Scan start : config file")
	file, ok := fileMap["/config"]
	if !ok {
		return nil, errors.New("config json file doesn't exist")
	}

	var d types.Image

	err = json.Unmarshal(file.Body, &d)
	if err != nil {
		return nil, errors.New("Fail to parse docker config file.")
	}

	return checkAssessments(d)
}

func checkAssessments(img types.Image) (assesses []*types.Assessment, err error) {
	if img.Config.User == "" || img.Config.User == "root" {
		assesses = append(assesses, &types.Assessment{
			Code:     types.AvoidRootDefault,
			Filename: ConfigFileName,
			Desc:     "Last user should not be root",
		})
	}

	for _, envVar := range img.Config.Env {
		e := strings.Split(envVar, "=")
		envKey := e[0]
		for _, suspiciousKey := range suspiciousEnvKey {
			if strings.Contains(envKey, suspiciousKey) {
				if _, ok := acceptanceEnvKey[envKey]; ok {
					continue
				}
				assesses = append(assesses, &types.Assessment{
					Code:     types.AvoidCredential,
					Filename: ConfigFileName,
					Desc:     fmt.Sprintf("Suspicious ENV key found : %s", envKey),
				})
			}
		}
	}

	if img.Config.Healthcheck == nil {
		assesses = append(assesses, &types.Assessment{
			Code:     types.AddHealthcheck,
			Filename: ConfigFileName,
			Desc:     "not found HEALTHCHECK statement",
		})
	}

	assessesCh := make(chan []*types.Assessment)
	for index, cmd := range img.History {
		go func(index int, cmd types.History) {
			assessesCh <- assessHistory(index, cmd)
		}(index, cmd)
	}

	timeout := time.After(10 * time.Second)
	for i := 0; i < len(img.History); i++ {
		select {
		case results := <-assessesCh:
			assesses = append(assesses, results...)
		case <-timeout:
			return nil, errors.New("timeout: manifest assessor")
		}
	}

	for volume := range img.Config.Volumes {
		if _, ok := sensitiveDirs[volume]; ok {
			assesses = append(assesses, &types.Assessment{
				Code:     types.AvoidSensitiveDirectoryMounting,
				Filename: ConfigFileName,
				Desc:     fmt.Sprintf("Avoid mounting sensitive dirs : %s", volume),
			})
		}
	}
	return assesses, nil
}

func splitByCommands(line string) map[int][]string {
	commands := strings.Split(line, "&&")

	tokens := map[int][]string{}
	for index, command := range commands {
		splitted := strings.Split(command, " ")
		cmds := []string{}
		for _, cmd := range splitted {
			trimmed := strings.TrimSpace(cmd)
			if trimmed != "" {
				cmds = append(cmds, trimmed)
			}

		}
		tokens[index] = cmds
	}
	return tokens
}

func assessHistory(index int, cmd types.History) []*types.Assessment {
	var assesses []*types.Assessment
	cmdSlices := splitByCommands(cmd.CreatedBy)
	if reducableApkAdd(cmdSlices) {
		assesses = append(assesses, &types.Assessment{
			Code:     types.UseApkAddNoCache,
			Filename: ConfigFileName,
			Desc:     fmt.Sprintf("Use --no-cache option if use 'apk add': %s", cmd.CreatedBy),
		})
	}

	if reducableAptGetInstall(cmdSlices) {
		assesses = append(assesses, &types.Assessment{
			Code:     types.MinimizeAptGet,
			Filename: ConfigFileName,
			Desc:     fmt.Sprintf("Use 'rm -rf /var/lib/apt/lists' after 'apt-get install' : %s", cmd.CreatedBy),
		})
	}

	if reducableAptGetUpdate(cmdSlices) {
		assesses = append(assesses, &types.Assessment{
			Code:     types.UseAptGetUpdateNoCache,
			Filename: ConfigFileName,
			Desc:     fmt.Sprintf("Always combine 'apt-get update' with 'apt-get install' : %s", cmd.CreatedBy),
		})
	}

	if index != 0 && useADDstatement(cmdSlices) {
		assesses = append(assesses, &types.Assessment{
			Code:     types.UseCOPY,
			Filename: ConfigFileName,
			Desc:     fmt.Sprintf("Use COPY : %s", cmd.CreatedBy),
		})
	}

	if useDistUpgrade(cmdSlices) {
		assesses = append(assesses, &types.Assessment{
			Code:     types.AvoidDistUpgrade,
			Filename: ConfigFileName,
			Desc:     fmt.Sprintf("Avoid upgrade in container : %s", cmd.CreatedBy),
		})
	}
	if useSudo(cmdSlices) {
		assesses = append(assesses, &types.Assessment{
			Code:     types.AvoidSudo,
			Filename: ConfigFileName,
			Desc:     fmt.Sprintf("Avoid sudo in container : %s", cmd.CreatedBy),
		})
	}

	return assesses
}

func useSudo(cmdSlices map[int][]string) bool {
	for _, cmdSlice := range cmdSlices {
		if containsAll(cmdSlice, []string{"sudo"}) {
			return true
		}
	}
	return false

}

func useDistUpgrade(cmdSlices map[int][]string) bool {
	for _, cmdSlice := range cmdSlices {
		if containsThreshold(cmdSlice, []string{"apt-get", "apt", "apk", "dist-upgrade"}, 2) {
			return true
		}
		if containsThreshold(cmdSlice, []string{"apt-get", "apt", "apk", "upgrade"}, 2) {
			return true
		}
	}
	return false
}

func useADDstatement(cmdSlices map[int][]string) bool {
	for _, cmdSlice := range cmdSlices {
		if containsAll(cmdSlice, []string{"ADD", "in"}) {
			return true
		}
	}
	return false
}

func reducableAptGetUpdate(cmdSlices map[int][]string) bool {
	var useAptUpdate bool
	var useAptInstall bool
	for _, cmdSlice := range cmdSlices {
		if !useAptUpdate && containsAll(cmdSlice, []string{"apt-get", "update"}) {
			useAptUpdate = true
		}

		if !useAptInstall && containsAll(cmdSlice, []string{"apt-get", "install"}) {
			useAptInstall = true
		}
		if useAptUpdate && useAptInstall {
			return false
		}
	}
	if useAptUpdate && !useAptInstall {
		return true
	}
	return false
}

func reducableAptGetInstall(cmdSlices map[int][]string) bool {
	var useAptInstall bool
	var useRmCache bool
	for _, cmdSlice := range cmdSlices {
		if !useAptInstall && containsAll(cmdSlice, []string{"apt-get", "install"}) {
			useAptInstall = true
		}
		if !useRmCache && containsThreshold(
			cmdSlice,
			[]string{"rm", "-rf", "-fr", "-r", "-fR", "/var/lib/apt/lists", "/var/lib/apt/lists/*", "/var/lib/apt/lists/*;"}, 3) {
			useRmCache = true
		}

		if useAptInstall && useRmCache {
			return false
		}
	}
	if useAptInstall && !useRmCache {
		return true
	}
	return false
}

func reducableApkAdd(cmdSlices map[int][]string) bool {
	for _, cmdSlice := range cmdSlices {
		if containsAll(cmdSlice, []string{"apk", "add"}) {
			if !containsAll(cmdSlice, []string{"--no-cache"}) {
				return true
			}
		}
	}
	return false
}

// manifest contains /config
func (a ManifestAssessor) RequiredFiles() []string {
	return []string{}
}

func (a ManifestAssessor) RequiredPermissions() []os.FileMode {
	return []os.FileMode{}
}

func containsAll(heystack []string, needles []string) bool {
	needleMap := map[string]struct{}{}
	for _, n := range needles {
		needleMap[n] = struct{}{}
	}

	for _, v := range heystack {
		if _, ok := needleMap[v]; ok {
			delete(needleMap, v)
			if len(needleMap) == 0 {
				return true
			}
		}
	}
	return false
}

func containsThreshold(heystack []string, needles []string, threshold int) bool {
	needleMap := map[string]struct{}{}
	for _, n := range needles {
		needleMap[n] = struct{}{}
	}

	existCnt := 0
	for _, v := range heystack {
		if _, ok := needleMap[v]; ok {
			delete(needleMap, v)
			existCnt++
			if existCnt >= threshold {
				return true
			}
		}
	}
	return false
}
