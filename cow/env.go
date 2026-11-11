package cow

import "os"

// repoEnvVars are the environment variables that make every Git child
// process act on a repository other than the one its working directory, or
// "-C", names. GIT_DIR and GIT_WORK_TREE override "-C", and GIT_INDEX_FILE
// and GIT_COMMON_DIR redirect the index and the shared Git directory.
var repoEnvVars = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"}

// refuseRepoEnv returns a refusal when any of repoEnvVars is set and not
// empty. They are refused and not removed, because removing them would act
// silently on a different repository than the caller named, for example with
// "git --git-dir=X cow add".
func refuseRepoEnv() error {
	for _, name := range repoEnvVars {
		if os.Getenv(name) != "" {
			return refusef("%s is set, and it would make git act on a different repository "+
				"than the one git-cow works on; unset it and run from inside the checkout "+
				"or use \"git -C <repo> cow ...\"", name)
		}
	}

	return nil
}
