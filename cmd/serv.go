// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2016 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package cmd

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"code.gitea.io/gitea/models"
	asymkey_model "code.gitea.io/gitea/models/asymkey"
	"code.gitea.io/gitea/models/perm"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/pprof"
	"code.gitea.io/gitea/modules/private"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/services/lfs"

	"github.com/golang-jwt/jwt/v4"
	"github.com/kballard/go-shellquote"
	"github.com/urfave/cli"
)

const (
	lfsAuthenticateVerb = "git-lfs-authenticate"
	gitAnnexShellVerb = "git-annex-shell"
)

// CmdServ represents the available serv sub-command.
var CmdServ = cli.Command{
	Name:        "serv",
	Usage:       "This command should only be called by SSH shell",
	Description: `Serv provide access auth for repositories`,
	Action:      runServ,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "enable-pprof",
		},
		cli.BoolFlag{
			Name: "debug",
		},
	},
}

func setup(logPath string, debug bool) {
	_ = log.DelLogger("console")
	if debug {
		_ = log.NewLogger(1000, "console", "console", `{"level":"trace","stacktracelevel":"NONE","stderr":true}`)
	} else {
		_ = log.NewLogger(1000, "console", "console", `{"level":"fatal","stacktracelevel":"NONE","stderr":true}`)
	}
	setting.LoadFromExisting()
	if debug {
		setting.RunMode = "dev"
	}
}

var (
	allowedCommands = map[string]perm.AccessMode{
		"git-upload-pack":    perm.AccessModeRead,
		"git-upload-archive": perm.AccessModeRead,
		"git-receive-pack":   perm.AccessModeWrite,
		lfsAuthenticateVerb:  perm.AccessModeNone,
		gitAnnexShellVerb:    perm.AccessModeNone,  // annex write access is allowed by unsetting GIT_ANNEX_SHELL_READONLY, when appropriate
	}
	alphaDashDotPattern = regexp.MustCompile(`[^\w-\.]`)
)

func fail(userMessage, logMessage string, args ...interface{}) error {
	// There appears to be a chance to cause a zombie process and failure to read the Exit status
	// if nothing is outputted on stdout.
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stderr, "Gitea:", userMessage)

	if len(logMessage) > 0 {
		if !setting.IsProd {
			fmt.Fprintf(os.Stderr, logMessage+"\n", args...)
		}
	}
	ctx, cancel := installSignals()
	defer cancel()

	if len(logMessage) > 0 {
		_ = private.SSHLog(ctx, true, fmt.Sprintf(logMessage+": ", args...))
	}
	return cli.NewExitError("", 1)
}

func runServ(c *cli.Context) error {
	ctx, cancel := installSignals()
	defer cancel()

	// FIXME: This needs to internationalised
	setup("serv.log", c.Bool("debug"))

	if setting.SSH.Disabled {
		println("Gitea: SSH has been disabled")
		return nil
	}

	if len(c.Args()) < 1 {
		if err := cli.ShowSubcommandHelp(c); err != nil {
			fmt.Printf("error showing subcommand help: %v\n", err)
		}
		return nil
	}

	keys := strings.Split(c.Args()[0], "-")
	if len(keys) != 2 || keys[0] != "key" {
		return fail("Key ID format error", "Invalid key argument: %s", c.Args()[0])
	}
	keyID, err := strconv.ParseInt(keys[1], 10, 64)
	if err != nil {
		return fail("Key ID format error", "Invalid key argument: %s", c.Args()[1])
	}

	cmd := os.Getenv("SSH_ORIGINAL_COMMAND")
	if len(cmd) == 0 {
		key, user, err := private.ServNoCommand(ctx, keyID)
		if err != nil {
			return fail("Internal error", "Failed to check provided key: %v", err)
		}
		switch key.Type {
		case asymkey_model.KeyTypeDeploy:
			println("Hi there! You've successfully authenticated with the deploy key named " + key.Name + ", but Gitea does not provide shell access.")
		case asymkey_model.KeyTypePrincipal:
			println("Hi there! You've successfully authenticated with the principal " + key.Content + ", but Gitea does not provide shell access.")
		default:
			println("Hi there, " + user.Name + "! You've successfully authenticated with the key named " + key.Name + ", but Gitea does not provide shell access.")
		}
		println("If this is unexpected, please log in with password and setup Gitea under another user.")
		return nil
	} else if c.Bool("debug") {
		log.Debug("SSH_ORIGINAL_COMMAND: %s", os.Getenv("SSH_ORIGINAL_COMMAND"))
	}

	words, err := shellquote.Split(cmd)
	if err != nil {
		return fail("Error parsing arguments", "Failed to parse arguments: %v", err)
	}

	if len(words) < 2 {
		if git.CheckGitVersionAtLeast("2.29") == nil {
			// for AGit Flow
			if cmd == "ssh_info" {
				fmt.Print(`{"type":"gitea","version":1}`)
				return nil
			}
		}
		return fail("Too few arguments", "Too few arguments in cmd: %s", cmd)
	}

	verb := words[0]
	repoPath := words[1]
	if repoPath[0] == '/' {
		repoPath = repoPath[1:]
	}

	var lfsVerb string
	if verb == lfsAuthenticateVerb {
		if !setting.LFS.StartServer {
			return fail("Unknown git command", "LFS authentication request over SSH denied, LFS support is disabled")
		}

		if len(words) > 2 {
			lfsVerb = words[2]
		}
	} else if verb == gitAnnexShellVerb {
		//if !setting.Annex.Enabled { // TODO
		if false {
			return fail("Unknown git command", "git-annex request over SSH denied, git-annex support is disabled")
		}

		// git-annex's args always put the repo in always words[2],
		// unlike most other git subcommands.
		// Also git-annex sometimes names repos like /~/, as if expecting $HOME to get expanded
		// e.g.:
		//   git-annex-shell 'configlist' '/~/user/repo'
		//   git-annex-shell 'sendkey' '/user/repo 'key'
		repoPath = words[2]
		repoPath = strings.TrimPrefix(repoPath, "/")
		repoPath = strings.TrimPrefix(repoPath, "~/")
	}

	// LowerCase and trim the repoPath as that's how they are stored.
	repoPath = strings.ToLower(strings.TrimSpace(repoPath))

	rr := strings.SplitN(repoPath, "/", 2)
	if len(rr) != 2 {
		return fail("Invalid repository path", "Invalid repository path: %v", repoPath)
	}

	username := strings.ToLower(rr[0])
	reponame := strings.ToLower(strings.TrimSuffix(rr[1], ".git"))

	if alphaDashDotPattern.MatchString(reponame) {
		return fail("Invalid repo name", "Invalid repo name: %s", reponame)
	}

	if c.Bool("enable-pprof") {
		if err := os.MkdirAll(setting.PprofDataPath, os.ModePerm); err != nil {
			return fail("Error while trying to create PPROF_DATA_PATH", "Error while trying to create PPROF_DATA_PATH: %v", err)
		}

		stopCPUProfiler, err := pprof.DumpCPUProfileForUsername(setting.PprofDataPath, username)
		if err != nil {
			return fail("Internal Server Error", "Unable to start CPU profile: %v", err)
		}
		defer func() {
			stopCPUProfiler()
			err := pprof.DumpMemProfileForUsername(setting.PprofDataPath, username)
			if err != nil {
				_ = fail("Internal Server Error", "Unable to dump Mem Profile: %v", err)
			}
		}()
	}

	requestedMode, has := allowedCommands[verb]
	if !has {
		return fail("Unknown git command", "Unknown git command %s", verb)
	}

	if verb == lfsAuthenticateVerb {
		if lfsVerb == "upload" {
			requestedMode = perm.AccessModeWrite
		} else if lfsVerb == "download" {
			requestedMode = perm.AccessModeRead
		} else {
			return fail("Unknown LFS verb", "Unknown lfs verb %s", lfsVerb)
		}
	} else if verb == gitAnnexShellVerb {
		gitAnnexVerb := words[1]

		// Compare how gitolite handles this: https://github.com/sitaramc/gitolite/blob/828152dc7f3ad421ff1eb50aeb982be664c95039/src/commands/git-annex-shell#L39-L47
		// > # Rather than keeping track of which git-annex-shell commands
		// > # require write access and which are readonly, we tell it
		// > # when readonly access is needed. [via GIT_ANNEX_SHELL_READONLY]
		// but gitea has separated permission checking behind the API layer (private.ServCommand)
		// which must be *told* what permission level the command requires,
		// instead of giving the subcommand itself a way check permissions.
		if gitAnnexVerb == "commit" {
			requestedMode = perm.AccessModeWrite
		} else if gitAnnexVerb == "configlist" {
			requestedMode = perm.AccessModeRead
		} else if gitAnnexVerb == "dropkey" {
			requestedMode = perm.AccessModeWrite
		} else if gitAnnexVerb == "gcryptsetup" {
			requestedMode = perm.AccessModeWrite
		} else if gitAnnexVerb == "inannex" {
			requestedMode = perm.AccessModeRead
		} else if gitAnnexVerb == "lockcontent" {
			requestedMode = perm.AccessModeWrite
		} else if gitAnnexVerb == "notifychanges" {
			requestedMode = perm.AccessModeRead
		} else if gitAnnexVerb == "p2pstdio" {
			requestedMode = perm.AccessModeWrite
		} else if gitAnnexVerb == "recvkey" {
			requestedMode = perm.AccessModeWrite
		} else if gitAnnexVerb == "sendkey" {
			requestedMode = perm.AccessModeRead
		} else if gitAnnexVerb == "transferinfo" {
			requestedMode = perm.AccessModeRead
		} else {
			return fail("Unknown annex verb", "Unknown annex verb %s", gitAnnexVerb)
		}
	}

	results, err := private.ServCommand(ctx, keyID, username, reponame, requestedMode, verb, lfsVerb)
	if err != nil {
		if private.IsErrServCommand(err) {
			errServCommand := err.(private.ErrServCommand)
			if errServCommand.StatusCode != http.StatusInternalServerError {
				return fail("Unauthorized", "%s", errServCommand.Error())
			}
			return fail("Internal Server Error", "%s", errServCommand.Error())
		}
		return fail("Internal Server Error", "%s", err.Error())
	}
	os.Setenv(models.EnvRepoIsWiki, strconv.FormatBool(results.IsWiki))
	os.Setenv(models.EnvRepoName, results.RepoName)
	os.Setenv(models.EnvRepoUsername, results.OwnerName)
	os.Setenv(models.EnvPusherName, results.UserName)
	os.Setenv(models.EnvPusherEmail, results.UserEmail)
	os.Setenv(models.EnvPusherID, strconv.FormatInt(results.UserID, 10))
	os.Setenv(models.EnvRepoID, strconv.FormatInt(results.RepoID, 10))
	os.Setenv(models.EnvPRID, fmt.Sprintf("%d", 0))
	os.Setenv(models.EnvDeployKeyID, fmt.Sprintf("%d", results.DeployKeyID))
	os.Setenv(models.EnvKeyID, fmt.Sprintf("%d", results.KeyID))
	os.Setenv(models.EnvAppURL, setting.AppURL)

	//LFS token authentication
	if verb == lfsAuthenticateVerb {
		url := fmt.Sprintf("%s%s/%s.git/info/lfs", setting.AppURL, url.PathEscape(results.OwnerName), url.PathEscape(results.RepoName))

		now := time.Now()
		claims := lfs.Claims{
			// FIXME: we need to migrate to RegisteredClaims
			StandardClaims: jwt.StandardClaims{ // nolint
				ExpiresAt: now.Add(setting.LFS.HTTPAuthExpiry).Unix(),
				NotBefore: now.Unix(),
			},
			RepoID: results.RepoID,
			Op:     lfsVerb,
			UserID: results.UserID,
		}
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

		// Sign and get the complete encoded token as a string using the secret
		tokenString, err := token.SignedString(setting.LFS.JWTSecretBytes)
		if err != nil {
			return fail("Internal error", "Failed to sign JWT token: %v", err)
		}

		tokenAuthentication := &models.LFSTokenResponse{
			Header: make(map[string]string),
			Href:   url,
		}
		tokenAuthentication.Header["Authorization"] = fmt.Sprintf("Bearer %s", tokenString)

		enc := json.NewEncoder(os.Stdout)
		err = enc.Encode(tokenAuthentication)
		if err != nil {
			return fail("Internal error", "Failed to encode LFS json response: %v", err)
		}
		return nil
	}

	// Special handle for Windows.
	if setting.IsWindows {
		verb = strings.Replace(verb, "-", " ", 1)
	}

	var gitcmd *exec.Cmd
	verbs := strings.Split(verb, " ")
	if len(verbs) == 2 {
		gitcmd = exec.CommandContext(ctx, verbs[0], verbs[1], repoPath)
	} else if verb == gitAnnexShellVerb {
		words[2] = setting.RepoRootPath + "/" + repoPath // git-annex-shell needs to be given an absolute path
		gitcmd = exec.CommandContext(ctx, verb, words[1:]...)
		gitcmd.Env = append(os.Environ(),
		  // "If set, disallows running git-shell to handle unknown commands."
		  // - git-annex-shell(1)
		  "GIT_ANNEX_SHELL_LIMITED=True",
		  // "If set, git-annex-shell will refuse to run commands
		  //  that do not operate on the specified directory."
		  // - git-annex-shell(1)
		  fmt.Sprintf("GIT_ANNEX_SHELL_DIRECTORY=%s", words[2]),
		  )
		if requestedMode < perm.AccessModeWrite {
		  // "If set, disallows any action that could modify the git-annex repository."
		  // - git-annex-shell(1)
		  //gitcmd.Env = append(gitcmd.Env, "GIT_ANNEX_SHELL_READONLY=True")
		}
	} else {
		gitcmd = exec.CommandContext(ctx, verb, repoPath)
	}

	gitcmd.Dir = setting.RepoRootPath
	gitcmd.Stdout = os.Stdout
	gitcmd.Stdin = os.Stdin
	gitcmd.Stderr = os.Stderr
	if err = gitcmd.Run(); err != nil {
		return fail("Internal error", "Failed to execute git command: %v", err)
	}

	// Update user key activity.
	if results.KeyID > 0 {
		if err = private.UpdatePublicKeyInRepo(ctx, results.KeyID, results.RepoID); err != nil {
			return fail("Internal error", "UpdatePublicKeyInRepo: %v", err)
		}
	}

	return nil
}
