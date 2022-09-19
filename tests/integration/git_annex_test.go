// Copyright 2022 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package integration

import (
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"

	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/perm"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/setting"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/tests"

	"github.com/stretchr/testify/require"
)

// Some guidelines:
//
// * a APITestContext is an awkward union of session credential + username + target repo
//   which is assumed to be owned by that username; if you want to target a different
//   repo, you need to edit its .Reponame or just ignore it and write "username/reponame.git"

func doCreateRemoteAnnexRepository(t *testing.T, u *url.URL, ctx APITestContext, private bool) (err error) {
	doAPICreateRepository(ctx, false)(t)
	doAPIEditRepository(ctx, &api.EditRepoOption{Private: &private})(t)

	repoURL := createSSHUrl(ctx.GitPath(), u)

	// Fill in fixture data
	withAnnexCtxKeyFile(t, ctx, func() {
		err = doInitRemoteAnnexRepository(t, repoURL)
	})
	if err != nil {
		return fmt.Errorf("Unable to initialize remote repo with git-annex fixture: %w", err)
	}
	return nil
}

/*
Test that permissions are enforced on git-annex-shell commands.

	Along the way, test that uploading, downloading, and deleting all work.
*/
func TestGitAnnexPermissions(t *testing.T) {
	if !setting.Annex.Enabled {
		t.Skip("Skipping since annex support is disabled.")
	}

	// Each case below is split so that 'clone' is done as
	// the repo owner, but 'copy' as the user under test.
	//
	// Otherwise, in cases where permissions block the
	// initial 'clone', the test would simply end there
	// and never verify if permissions apply properly to
	// 'annex copy' -- potentially leaving a security gap.

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		t.Run("Public", func(t *testing.T) {
			defer tests.PrintCurrentTest(t)()

			ownerCtx := NewAPITestContext(t, "user2", "annex-public", auth_model.AccessTokenScopeRepo, auth_model.AccessTokenScopeDeleteRepo)

			// create a public repo
			require.NoError(t, doCreateRemoteAnnexRepository(t, u, ownerCtx, false))

			// double-check it's public
			repo, err := repo_model.GetRepositoryByOwnerAndName(db.DefaultContext, ownerCtx.Username, ownerCtx.Reponame)
			require.NoError(t, err)
			require.False(t, repo.IsPrivate)

			// Remote addresses of the repo
			repoURL := createSSHUrl(ownerCtx.GitPath(), u)                        // remote git URL
			remoteRepoPath := path.Join(setting.RepoRootPath, ownerCtx.GitPath()) // path on disk -- which can be examined directly because we're testing from localhost

			// Different sessions, so we can test different permissions.
			// We leave Reponame blank because we don't actually then later add it according to each case if needed
			//
			// NB: these usernames need to match appropriate entries in models/fixtures/user.yml
			writerCtx := NewAPITestContext(t, "user5", "", auth_model.AccessTokenScopeRepo)
			readerCtx := NewAPITestContext(t, "user4", "", auth_model.AccessTokenScopeRepo)
			outsiderCtx := NewAPITestContext(t, "user8", "", auth_model.AccessTokenScopeRepo) // a user with no specific access

			// set up collaborators
			doAPIAddCollaborator(ownerCtx, readerCtx.Username, perm.AccessModeRead)(t)
			doAPIAddCollaborator(ownerCtx, writerCtx.Username, perm.AccessModeWrite)(t)

			// tests
			t.Run("Owner", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})

						t.Run("Upload", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexUploadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Writer", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxKeyFile(t, writerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})

						t.Run("Upload", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexUploadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Reader", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxKeyFile(t, readerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})

						t.Run("Upload", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.Error(t, doAnnexUploadTest(remoteRepoPath, repoPath), "Uploading should fail due to permissions")
						})
					})
				})
			})

			t.Run("Outsider", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxKeyFile(t, outsiderCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})

						t.Run("Upload", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.Error(t, doAnnexUploadTest(remoteRepoPath, repoPath), "Uploading should fail due to permissions")
						})
					})
				})
			})

			t.Run("Delete", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				// Delete the repo, make sure it's fully gone
				doAPIDeleteRepository(ownerCtx)(t)
				_, statErr := os.Stat(remoteRepoPath)
				require.True(t, os.IsNotExist(statErr), "Remote annex repo should be removed from disk")
			})
		})

		t.Run("Private", func(t *testing.T) {
			defer tests.PrintCurrentTest(t)()

			ownerCtx := NewAPITestContext(t, "user2", "annex-private", auth_model.AccessTokenScopeRepo, auth_model.AccessTokenScopeDeleteRepo)

			// create a private repo
			require.NoError(t, doCreateRemoteAnnexRepository(t, u, ownerCtx, true))

			// double-check it's private
			repo, err := repo_model.GetRepositoryByOwnerAndName(db.DefaultContext, ownerCtx.Username, ownerCtx.Reponame)
			require.NoError(t, err)
			require.True(t, repo.IsPrivate)

			// Remote addresses of the repo
			repoURL := createSSHUrl(ownerCtx.GitPath(), u)                        // remote git URL
			remoteRepoPath := path.Join(setting.RepoRootPath, ownerCtx.GitPath()) // path on disk -- which can be examined directly because we're testing from localhost

			// Different sessions, so we can test different permissions.
			// We leave Reponame blank because we don't actually then later add it according to each case if needed
			//
			// NB: these usernames need to match appropriate entries in models/fixtures/user.yml
			writerCtx := NewAPITestContext(t, "user5", "", auth_model.AccessTokenScopeRepo)
			readerCtx := NewAPITestContext(t, "user4", "", auth_model.AccessTokenScopeRepo)
			outsiderCtx := NewAPITestContext(t, "user8", "", auth_model.AccessTokenScopeRepo) // a user with no specific access
			// Note: there's also full anonymous access, which is only available for public HTTP repos;
			// it should behave the same as 'outsider' but we (will) test it separately below anyway

			// set up collaborators
			doAPIAddCollaborator(ownerCtx, readerCtx.Username, perm.AccessModeRead)(t)
			doAPIAddCollaborator(ownerCtx, writerCtx.Username, perm.AccessModeWrite)(t)

			// tests
			t.Run("Owner", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})

						t.Run("Upload", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexUploadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Writer", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxKeyFile(t, writerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})

						t.Run("Upload", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexUploadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Reader", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxKeyFile(t, readerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})

						t.Run("Upload", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.Error(t, doAnnexUploadTest(remoteRepoPath, repoPath), "Uploading should fail due to permissions")
						})
					})
				})
			})

			t.Run("Outsider", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxKeyFile(t, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxKeyFile(t, outsiderCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.Error(t, doAnnexInitTest(remoteRepoPath, repoPath), "annex init should fail due to permissions")
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.Error(t, doAnnexDownloadTest(remoteRepoPath, repoPath), "annex copy --from should fail due to permissions")
						})

						t.Run("Upload", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()

							require.Error(t, doAnnexUploadTest(remoteRepoPath, repoPath), "annex copy --to should fail due to permissions")
						})
					})
				})
			})

			t.Run("Delete", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				// Delete the repo, make sure it's fully gone
				doAPIDeleteRepository(ownerCtx)(t)
				_, statErr := os.Stat(remoteRepoPath)
				require.True(t, os.IsNotExist(statErr), "Remote annex repo should be removed from disk")
			})
		})
	})
}

/*
Test that 'git annex init' works.

	precondition: repoPath contains a pre-cloned repo set up by doInitAnnexRepository().
*/
func doAnnexInitTest(remoteRepoPath, repoPath string) (err error) {
	_, _, err = git.NewCommand(git.DefaultContext, "annex", "init", "cloned-repo").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return fmt.Errorf("Couldn't `git annex init`: %w", err)
	}

	// - method 0: 'git config remote.origin.annex-uuid'.
	//   Demonstrates that 'git annex init' successfully contacted
	//   the remote git-annex and was able to learn its ID number.
	readAnnexUUID, _, err := git.NewCommand(git.DefaultContext, "config", "remote.origin.annex-uuid").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return fmt.Errorf("Couldn't read remote `git config remote.origin.annex-uuid`: %w", err)
	}
	readAnnexUUID = strings.TrimSpace(readAnnexUUID)

	match := regexp.MustCompile("^[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}$").MatchString(readAnnexUUID)
	if !match {
		return fmt.Errorf("'git config remote.origin.annex-uuid' should have been able to download the remote's uuid; but instead read '%s'", readAnnexUUID)
	}

	remoteAnnexUUID, _, err := git.NewCommand(git.DefaultContext, "config", "annex.uuid").RunStdString(&git.RunOpts{Dir: remoteRepoPath})
	if err != nil {
		return fmt.Errorf("Couldn't read local `git config annex.uuid`: %w", err)
	}

	remoteAnnexUUID = strings.TrimSpace(remoteAnnexUUID)
	match = regexp.MustCompile("^[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}$").MatchString(remoteAnnexUUID)
	if !match {
		return fmt.Errorf("'git annex init' should have been able to download the remote's uuid; but instead read '%s'", remoteAnnexUUID)
	}

	if readAnnexUUID != remoteAnnexUUID {
		return fmt.Errorf("'git annex init' should have read the expected annex UUID '%s', but instead got '%s'", remoteAnnexUUID, readAnnexUUID)
	}

	// - method 1: 'git annex whereis'.
	//   Demonstrates that git-annex understands the annexed file can be found in the remote annex.
	annexWhereis, _, err := git.NewCommand(git.DefaultContext, "annex", "whereis", "large.bin").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return fmt.Errorf("Couldn't `git annex whereis large.bin`: %w", err)
	}
	// Note: this regex is unanchored because 'whereis' outputs multiple lines containing
	//       headers and 1+ remotes and we just want to find one of them.
	match = regexp.MustCompile(regexp.QuoteMeta(remoteAnnexUUID) + " -- .* \\[origin\\]\n").MatchString(annexWhereis)
	if !match {
		return errors.New("'git annex whereis' should report large.bin is known to be in [origin]")
	}

	return nil
}

func doAnnexDownloadTest(remoteRepoPath, repoPath string) (err error) {
	// NB: this test does something slightly different if run separately from "doAnnexInitTest()":
	//     "git annex copy" will notice and run "git annex init", silently.
	//     This shouldn't change any results, but be aware in case it does.

	_, _, err = git.NewCommand(git.DefaultContext, "annex", "copy", "--from", "origin").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	// verify the file was downloaded
	localObjectPath, err := annexObjectPath(repoPath, "large.bin")
	if err != nil {
		return err
	}
	// localObjectPath := path.Join(repoPath, "large.bin") // or, just compare against the checked-out file

	remoteObjectPath, err := annexObjectPath(remoteRepoPath, "large.bin")
	if err != nil {
		return err
	}

	match, err := util.FileCmp(localObjectPath, remoteObjectPath, 0)
	if err != nil {
		return err
	}
	if !match {
		return errors.New("Annexed files should be the same")
	}

	return nil
}

func doAnnexUploadTest(remoteRepoPath, repoPath string) (err error) {
	// NB: this test does something slightly different if run separately from "Init":
	//     it first runs "git annex init" silently in the background.
	//     This shouldn't change any results, but be aware in case it does.

	err = generateRandomFile(1024*1024/4, path.Join(repoPath, "contribution.bin"))
	if err != nil {
		return err
	}

	err = git.AddChanges(repoPath, false, ".")
	if err != nil {
		return err
	}

	err = git.CommitChanges(repoPath, git.CommitChangesOptions{Message: "Annex another file"})
	if err != nil {
		return err
	}

	_, _, err = git.NewCommand(git.DefaultContext, "annex", "copy", "--to", "origin").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	_, _, err = git.NewCommand(git.DefaultContext, "annex", "sync", "--no-content").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	// verify the file was uploaded
	localObjectPath, err := annexObjectPath(repoPath, "contribution.bin")
	if err != nil {
		return err
	}
	// localObjectPath := path.Join(repoPath, "contribution.bin") // or, just compare against the checked-out file

	remoteObjectPath, err := annexObjectPath(remoteRepoPath, "contribution.bin")
	if err != nil {
		return err
	}

	match, err := util.FileCmp(localObjectPath, remoteObjectPath, 0)
	if err != nil {
		return err
	}
	if !match {
		return errors.New("Annexed files should be the same")
	}

	return nil
}

// ---- Helpers ----

func generateRandomFile(size int, path string) (err error) {
	// Generate random file

	// XXX TODO: maybe this should not be random, but instead a predictable pattern, so that the test is deterministic
	bufSize := 4 * 1024
	if bufSize > size {
		bufSize = size
	}

	buffer := make([]byte, bufSize)

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	written := 0
	for written < size {
		n := size - written
		if n > bufSize {
			n = bufSize
		}
		_, err := rand.Read(buffer[:n])
		if err != nil {
			return err
		}
		n, err = f.Write(buffer[:n])
		if err != nil {
			return err
		}
		written += n
	}
	if err != nil {
		return err
	}

	return nil
}

// ---- Annex-specific helpers ----

/*
Initialize a repo with some baseline annexed and non-annexed files.

	TODO: perhaps this generator could be replaced with a fixture (see
	integrations/gitea-repositories-meta/ and models/fixtures/repository.yml).
	However we reuse this template for -different- repos, so maybe not.
*/
func doInitAnnexRepository(repoPath string) error {
	// set up what files should be annexed
	// in this case, all *.bin  files will be annexed
	// without this, git-annex's default config annexes every file larger than some number of megabytes
	f, err := os.Create(path.Join(repoPath, ".gitattributes"))
	if err != nil {
		return err
	}
	defer f.Close()

	// set up git-annex to store certain filetypes via *annex* pointers
	// (https://git-annex.branchable.com/internals/pointer_file/).
	// but only when run via 'git add' (see git-annex-smudge(1))
	_, err = f.WriteString("*                   annex.largefiles=anything\n")
	if err != nil {
		return err
	}
	_, err = f.WriteString("*.bin  filter=annex\n")
	if err != nil {
		return err
	}
	f.Close()

	err = git.AddChanges(repoPath, false, ".")
	if err != nil {
		return err
	}
	err = git.CommitChanges(repoPath, git.CommitChangesOptions{Message: "Configure git-annex settings"})
	if err != nil {
		return err
	}

	// 'git annex init'
	err = git.NewCommand(git.DefaultContext, "annex", "init", "test-repo").Run(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	// add a file to the annex
	err = generateRandomFile(1024*1024/4, path.Join(repoPath, "large.bin"))
	if err != nil {
		return err
	}
	err = git.AddChanges(repoPath, false, ".")
	if err != nil {
		return err
	}
	err = git.CommitChanges(repoPath, git.CommitChangesOptions{Message: "Annex a file"})
	if err != nil {
		return err
	}

	return nil
}

/*
Initialize a remote repo with some baseline annexed and non-annexed files.
*/
func doInitRemoteAnnexRepository(t *testing.T, repoURL *url.URL) error {
	repoPath := path.Join(t.TempDir(), path.Base(repoURL.Path))
	// This clone is immediately thrown away, which
	// helps force the tests to be end-to-end.
	defer util.RemoveAll(repoPath)

	doGitClone(repoPath, repoURL)(t) // TODO: this call is the only reason for the testing.T; can it be removed?

	err := doInitAnnexRepository(repoPath)
	if err != nil {
		return err
	}

	_, _, err = git.NewCommand(git.DefaultContext, "annex", "sync", "--content").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	return nil
}

/*
Find the path in .git/annex/objects/ of the contents for a given annexed file.

	repoPath: the git repository to examine
	file: the path (in the repo's current HEAD) of the annex pointer

	TODO: pass a parameter to allow examining non-HEAD branches
*/
func annexObjectPath(repoPath, file string) (string, error) {
	// NB: `git annex lookupkey` is more reliable, but doesn't work in bare repos.
	annexKey, _, err := git.NewCommandContextNoGlobals(git.DefaultContext, "show").AddDynamicArguments("HEAD:" + file).RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return "", fmt.Errorf("in %s: %w", repoPath, err) // the error from git prints the filename but not repo
	}

	// There are two formats an annexed file pointer might be:
	// * a symlink to .git/annex/objects/$HASHDIR/$ANNEX_KEY/$ANNEX_KEY - used by files created with 'git annex add'
	// * a text file containing /annex/objects/$ANNEX_KEY - used by files for which 'git add' was configured to run git-annex-smudge
	// This recovers $ANNEX_KEY from either case:
	annexKey = path.Base(strings.TrimSpace(annexKey))

	contentPath, _, err := git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "contentlocation").AddDynamicArguments(annexKey).RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return "", fmt.Errorf("in %s: %s does not seem to be annexed: %w", repoPath, file, err)
	}
	contentPath = strings.TrimSpace(contentPath)

	return path.Join(repoPath, contentPath), nil
}

/* like withKeyFile(), but automatically sets it the account given in ctx for use by git-annex */
func withAnnexCtxKeyFile(t *testing.T, ctx APITestContext, callback func()) {
	_gitAnnexUseGitSSH, gitAnnexUseGitSSHExists := os.LookupEnv("GIT_ANNEX_USE_GIT_SSH")
	defer func() {
		// reset
		if gitAnnexUseGitSSHExists {
			os.Setenv("GIT_ANNEX_USE_GIT_SSH", _gitAnnexUseGitSSH)
		}
	}()

	os.Setenv("GIT_ANNEX_USE_GIT_SSH", "1") // withKeyFile works by setting GIT_SSH_COMMAND, but git-annex only respects that if this is set

	withCtxKeyFile(t, ctx, callback)
}
