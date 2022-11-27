// Copyright 2022 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package integration

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
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
	"code.gitea.io/gitea/modules/annex"
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

func TestGitAnnexMedia(t *testing.T) {
	if !setting.Annex.Enabled {
		t.Skip("Skipping since annex support is disabled.")
	}

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		ctx := NewAPITestContext(t, "user2", "annex-media-test", auth_model.AccessTokenScopeRepo)

		// create a public repo
		require.NoError(t, doCreateRemoteAnnexRepository(t, u, ctx, false))

		// the filenames here correspond to specific cases defined in doInitAnnexRepository()
		t.Run("AnnexSymlink", func(t *testing.T) {
			defer tests.PrintCurrentTest(t)()
			doAnnexMediaTest(t, ctx, "annexed.tiff")
		})
		t.Run("AnnexPointer", func(t *testing.T) {
			defer tests.PrintCurrentTest(t)()
			doAnnexMediaTest(t, ctx, "annexed.bin")
		})
	})
}

func doAnnexMediaTest(t *testing.T, ctx APITestContext, file string) {
	// Make sure that downloading via /media on the website recognizes it should give the annexed content

	// TODO:
	// - [ ] roll this into TestGitAnnexPermissions to ensure that permission enforcement works correctly even on /media?

	session := loginUser(t, ctx.Username) // logs in to the http:// site/API, storing a cookie;
	// this is a different auth method than the git+ssh:// or git+http:// protocols TestGitAnnexPermissions uses!

	// compute server-side path of the annexed file
	remoteRepoPath := path.Join(setting.RepoRootPath, ctx.GitPath())
	remoteObjectPath, err := contentLocation(remoteRepoPath, file)
	require.NoError(t, err)

	// download annexed file
	localObjectPath := path.Join(t.TempDir(), file)
	fd, err := os.OpenFile(localObjectPath, os.O_CREATE|os.O_WRONLY, 0o777)
	defer fd.Close()
	require.NoError(t, err)

	mediaLink := path.Join("/", ctx.Username, ctx.Reponame, "/media/branch/master", file)
	req := NewRequest(t, "GET", mediaLink)
	resp := session.MakeRequest(t, req, http.StatusOK)

	_, err = io.Copy(fd, resp.Body)
	require.NoError(t, err)
	fd.Close()

	// verify the download
	match, err := util.FileCmp(localObjectPath, remoteObjectPath, 0)
	require.NoError(t, err)
	require.True(t, match, "Annexed files should be the same")
}

func TestGitAnnexViews(t *testing.T) {
	if !setting.Annex.Enabled {
		t.Skip("Skipping since annex support is disabled.")
	}

	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		ctx := NewAPITestContext(t, "user2", "annex-template-render-test", auth_model.AccessTokenScopeRepo)

		// create a public repo
		require.NoError(t, doCreateRemoteAnnexRepository(t, u, ctx, false))

		session := loginUser(t, ctx.Username)

		t.Run("Index", func(t *testing.T) {
			// test that annex symlinks renders with the _file icon_ on the main list
			defer tests.PrintCurrentTest(t)()

			repoLink := path.Join("/", ctx.Username, ctx.Reponame)
			req := NewRequest(t, "GET", repoLink)
			resp := session.MakeRequest(t, req, http.StatusOK)

			htmlDoc := NewHTMLParser(t, resp.Body)
			isFileIcon := htmlDoc.Find("tr[data-entryname='annexed.tiff'] > td.name svg").HasClass("octicon-file")
			require.True(t, isFileIcon, "annexed files should render a plain file icon, even when stored via annex symlink")
		})

		t.Run("View", func(t *testing.T) {
			// test how routers/web/repo/view.go + templates/repo/view_file.tmpl handle annexed files
			defer tests.PrintCurrentTest(t)()

			doViewTest := func(file string) (htmlDoc *HTMLDoc, viewLink, mediaLink string) {
				viewLink = path.Join("/", ctx.Username, ctx.Reponame, "/src/branch/master", file)
				// rawLink := strings.Replace(viewLink, "/src/", "/raw/", 1) // TODO: do something with this?
				mediaLink = strings.Replace(viewLink, "/src/", "/media/", 1)

				req := NewRequest(t, "GET", viewLink)
				resp := session.MakeRequest(t, req, http.StatusOK)

				htmlDoc = NewHTMLParser(t, resp.Body)
				// the first button on the toolbar on the view template is the "Raw" button
				// this CSS selector is the most precise I can think to use
				buttonLink, exists := htmlDoc.Find(".file-header").Find("a[download]").Attr("href")
				require.True(t, exists, "Download button should exist on the file header")
				require.EqualValues(t, mediaLink, buttonLink, "Download link should use /media URL for annex files")

				return
			}

			t.Run("Binary", func(t *testing.T) {
				// test that annexing a file renders the /media link in /src and NOT the /raw link
				defer tests.PrintCurrentTest(t)()

				doBinaryViewTest := func(file string) {
					htmlDoc, _, mediaLink := doViewTest(file)

					rawLink, exists := htmlDoc.Find("div.file-view > div.view-raw > a").Attr("href")
					require.True(t, exists, "Download link should render instead of content because this is a binary file")
					require.EqualValues(t, mediaLink, rawLink)
				}

				t.Run("AnnexSymlink", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()
					doBinaryViewTest("annexed.tiff")
				})
				t.Run("AnnexPointer", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()
					doBinaryViewTest("annexed.bin")
				})
			})

			t.Run("Text", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				doTextViewTest := func(file string) {
					htmlDoc, _, _ := doViewTest(file)
					require.True(t, htmlDoc.Find("div.file-view").Is(".code-view"), "should render as code")
				}

				t.Run("AnnexSymlink", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()
					doTextViewTest("annexed.txt")

					t.Run("Markdown", func(t *testing.T) {
						// special case: check that markdown can be pulled out of the annex and rendered, too
						defer tests.PrintCurrentTest(t)()
						htmlDoc, _, _ := doViewTest("annexed.md")
						require.True(t, htmlDoc.Find("div.file-view").Is(".markdown"), "should render as markdown")
					})
				})
				t.Run("AnnexPointer", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()
					doTextViewTest("annexed.rst")

					t.Run("Markdown", func(t *testing.T) {
						// special case: check that markdown can be pulled out of the annex and rendered, too
						defer tests.PrintCurrentTest(t)()
						htmlDoc, _, _ := doViewTest("annexed.markdown")
						require.True(t, htmlDoc.Find("div.file-view").Is(".markdown"), "should render as markdown")
					})
				})
			})
		})
	})
}

/*
Test that permissions are enforced on git-annex-shell commands.

	Along the way, this also tests that uploading, downloading, and deleting all work,
	so we haven't written separate tests for those.
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
		// Tell git-annex to allow http://127.0.0.1, http://localhost and http://::1. Without
		// this, all `git annex` commands will silently fail when run against http:// remotes
		// without explaining what's wrong.
		//
		// Note: onGiteaRun() sets up an alternate HOME so this actually edits
		//       tests/integration/gitea-integration-*/data/home/.gitconfig and
		//       if you're debugging you need to remember to match that.
		_, _, err := git.NewCommandContextNoGlobals(git.DefaultContext, "config").AddOptionValues("--global").AddArguments("annex.security.allowed-ip-addresses", "all").RunStdString(&git.RunOpts{})
		require.NoError(t, err)

		t.Run("Public", func(t *testing.T) {
			defer tests.PrintCurrentTest(t)()

			ownerCtx := NewAPITestContext(t, "user2", "annex-public", auth_model.AccessTokenScopeRepo, auth_model.AccessTokenScopeDeleteRepo)

			// create a public repo
			require.NoError(t, doCreateRemoteAnnexRepository(t, u, ownerCtx, false))

			// double-check it's public
			repo, err := repo_model.GetRepositoryByOwnerAndName(db.DefaultContext, ownerCtx.Username, ownerCtx.Reponame)
			require.NoError(t, err)
			require.False(t, repo.IsPrivate)

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

					repoURL := createSSHUrl(ownerCtx.GitPath(), u)

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

				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Writer", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createSSHUrl(ownerCtx.GitPath(), u)

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

				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxHTTPPassword(t, u, writerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Reader", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createSSHUrl(ownerCtx.GitPath(), u)

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

				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxHTTPPassword(t, u, readerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Outsider", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createSSHUrl(ownerCtx.GitPath(), u)

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

				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxHTTPPassword(t, u, outsiderCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Anonymous", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				// Only HTTP has an anonymous mode
				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					// unlike the other tests, at this step we *do not* define credentials:

					t.Run("Init", func(t *testing.T) {
						defer tests.PrintCurrentTest(t)()
						require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
					})

					t.Run("Download", func(t *testing.T) {
						defer tests.PrintCurrentTest(t)()
						require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
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

					repoURL := createSSHUrl(ownerCtx.GitPath(), u)

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

				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Writer", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createSSHUrl(ownerCtx.GitPath(), u)

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

				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxHTTPPassword(t, u, writerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Reader", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createSSHUrl(ownerCtx.GitPath(), u)

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

				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxHTTPPassword(t, u, readerCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.NoError(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Outsider", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				t.Run("SSH", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createSSHUrl(ownerCtx.GitPath(), u)

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

				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					withAnnexCtxHTTPPassword(t, u, outsiderCtx, func() {
						t.Run("Init", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.Error(t, doAnnexInitTest(remoteRepoPath, repoPath))
						})

						t.Run("Download", func(t *testing.T) {
							defer tests.PrintCurrentTest(t)()
							require.Error(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
						})
					})
				})
			})

			t.Run("Anonymous", func(t *testing.T) {
				defer tests.PrintCurrentTest(t)()

				// Only HTTP has an anonymous mode
				t.Run("HTTP", func(t *testing.T) {
					defer tests.PrintCurrentTest(t)()

					repoURL := createHTTPUrl(ownerCtx.GitPath(), u)

					repoPath := path.Join(t.TempDir(), ownerCtx.Reponame)
					defer util.RemoveAll(repoPath) // cleans out git-annex lockdown permissions

					withAnnexCtxHTTPPassword(t, u, ownerCtx, func() {
						doGitClone(repoPath, repoURL)(t)
					})

					// unlike the other tests, at this step we *do not* define credentials:

					t.Run("Init", func(t *testing.T) {
						defer tests.PrintCurrentTest(t)()
						require.Error(t, doAnnexInitTest(remoteRepoPath, repoPath))
					})

					t.Run("Download", func(t *testing.T) {
						defer tests.PrintCurrentTest(t)()
						require.Error(t, doAnnexDownloadTest(remoteRepoPath, repoPath))
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
	_, _, err = git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "init", "cloned-repo").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return fmt.Errorf("Couldn't `git annex init`: %w", err)
	}

	// - method 0: 'git config remote.origin.annex-uuid'.
	//   Demonstrates that 'git annex init' successfully contacted
	//   the remote git-annex and was able to learn its ID number.
	readAnnexUUID, _, err := git.NewCommandContextNoGlobals(git.DefaultContext, "config", "remote.origin.annex-uuid").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return fmt.Errorf("Couldn't read remote `git config remote.origin.annex-uuid`: %w", err)
	}
	readAnnexUUID = strings.TrimSpace(readAnnexUUID)

	match := regexp.MustCompile("^[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}$").MatchString(readAnnexUUID)
	if !match {
		return fmt.Errorf("'git config remote.origin.annex-uuid' should have been able to download the remote's uuid; but instead read '%s'", readAnnexUUID)
	}

	remoteAnnexUUID, _, err := git.NewCommandContextNoGlobals(git.DefaultContext, "config", "annex.uuid").RunStdString(&git.RunOpts{Dir: remoteRepoPath})
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
	//   Demonstrates that git-annex understands annexed files can be found in the remote annex.
	annexWhereis, _, err := git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "whereis", "annexed.bin").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return fmt.Errorf("Couldn't `git annex whereis`: %w", err)
	}
	// Note: this regex is unanchored because 'whereis' outputs multiple lines containing
	//       headers and 1+ remotes and we just want to find one of them.
	match = regexp.MustCompile(regexp.QuoteMeta(remoteAnnexUUID) + " -- .* \\[origin\\]\n").MatchString(annexWhereis)
	if !match {
		return errors.New("'git annex whereis' should report files are known to be in [origin]")
	}

	return nil
}

func doAnnexDownloadTest(remoteRepoPath, repoPath string) (err error) {
	// NB: this test does something slightly different if run separately from "doAnnexInitTest()":
	//     "git annex copy" will notice and run "git annex init", silently.
	//     This shouldn't change any results, but be aware in case it does.

	_, _, err = git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "copy", "--from", "origin").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	// verify the files downloaded

	cmp := func(filename string) error {
		localObjectPath, err := contentLocation(repoPath, filename)
		if err != nil {
			return err
		}
		// localObjectPath := path.Join(repoPath, filename) // or, just compare against the checked-out file

		remoteObjectPath, err := contentLocation(remoteRepoPath, filename)
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

	// this is the annex-symlink file
	stat, err := os.Lstat(path.Join(repoPath, "annexed.tiff"))
	if err != nil {
		return fmt.Errorf("Lstat: %w", err)
	}
	if !((stat.Mode() & os.ModeSymlink) != 0) {
		// this line is really just double-checking that the text fixture is set up correctly
		return errors.New("*.tiff should be a symlink")
	}
	if err = cmp("annexed.tiff"); err != nil {
		return err
	}

	// this is the annex-pointer file
	stat, err = os.Lstat(path.Join(repoPath, "annexed.bin"))
	if err != nil {
		return fmt.Errorf("Lstat: %w", err)
	}
	if !((stat.Mode() & os.ModeSymlink) == 0) {
		// this line is really just double-checking that the text fixture is set up correctly
		return errors.New("*.bin should not be a symlink")
	}
	err = cmp("annexed.bin")

	return err
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

	_, _, err = git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "copy", "--to", "origin").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	_, _, err = git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "sync", "--no-content").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	// verify the file was uploaded
	localObjectPath, err := contentLocation(repoPath, "contribution.bin")
	if err != nil {
		return err
	}
	// localObjectPath := path.Join(repoPath, "contribution.bin") // or, just compare against the checked-out file

	remoteObjectPath, err := contentLocation(remoteRepoPath, "contribution.bin")
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
	_, err = f.WriteString("*.rst  filter=annex\n")
	if err != nil {
		return err
	}
	_, err = f.WriteString("*.markdown  filter=annex\n")
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
	err = git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "init", "test-repo").Run(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	// add files to the annex, stored via annex symlinks
	// // a binary file
	err = generateRandomFile(1024*1024/4, path.Join(repoPath, "annexed.tiff"))
	if err != nil {
		return err
	}

	// // a text file
	err = os.WriteFile(path.Join(repoPath, "annexed.md"), []byte("Overview\n=====\n\n1. Profit\n2. ???\n3. Review Life Activations\n"), 0o777)
	if err != nil {
		return err
	}

	// // a markdown file
	err = os.WriteFile(path.Join(repoPath, "annexed.txt"), []byte("We're going to see the wizard\nThe wonderful\nMonkey of\nBoz\n"), 0o777)
	if err != nil {
		return err
	}

	err = git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "add", ".").Run(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return err
	}

	// add files to the annex, stored via git-annex-smudge
	// // a binary file
	err = generateRandomFile(1024*1024/4, path.Join(repoPath, "annexed.bin"))
	if err != nil {
		return err
	}

	// // a text file
	err = os.WriteFile(path.Join(repoPath, "annexed.rst"), []byte("Title\n=====\n\n- this is to test annexing a text file\n- lists are fun\n"), 0o777)
	if err != nil {
		return err
	}

	// // a markdown file
	err = os.WriteFile(path.Join(repoPath, "annexed.markdown"), []byte("Overview\n=====\n\n1. Profit\n2. ???\n3. Review Life Activations\n"), 0o777)
	if err != nil {
		return err
	}

	err = git.AddChanges(repoPath, false, ".")
	if err != nil {
		return err
	}

	// save everything
	err = git.CommitChanges(repoPath, git.CommitChangesOptions{Message: "Annex files"})
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

	_, _, err = git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "sync", "--content").RunStdString(&git.RunOpts{Dir: repoPath})
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
func contentLocation(repoPath, file string) (path string, err error) {
	path = ""

	repo, err := git.OpenRepository(git.DefaultContext, repoPath)
	if err != nil {
		return
	}

	commitID, err := repo.GetRefCommitID("HEAD") // NB: to examine a *branch*, prefix with "refs/branch/", or call repo.GetBranchCommitID(); ditto for tags
	if err != nil {
		return
	}

	commit, err := repo.GetCommit(commitID)
	if err != nil {
		return
	}

	treeEntry, err := commit.GetTreeEntryByPath(file)
	if err != nil {
		return
	}

	return annex.ContentLocation(treeEntry.Blob())
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

/*
Like withKeyFile(), but sets HTTP credentials instead of SSH credentials.

	It does this by temporarily arranging through `git config --global`
	to use git-credential-store(1) with the password written to a tempfile.

	This is the only reliable way to pass HTTP credentials non-interactively
	to git-annex.  See https://git-annex.branchable.com/bugs/http_remotes_ignore_annex.web-options_--netrc/#comment-b5a299e9826b322f2d85c96d4929a430
	for joeyh's proclamation on the subject.

	This **is only effective** when used around git.NewCommandContextNoGlobals() calls.
	git.NewCommand() disables credential.helper as a precaution (see modules/git/git.go).

	In contrast, the tests in git_test.go put the password in the remote's URL like
	`git config remote.origin.url http://user2:password@localhost:3003/user2/repo-name.git`,
	writing the password in repoPath+"/.git/config". That would be equally good, except
	that git-annex ignores it!
*/
func withAnnexCtxHTTPPassword(t *testing.T, u *url.URL, ctx APITestContext, callback func()) {
	credentialedURL := *u
	credentialedURL.User = url.UserPassword(ctx.Username, userPassword) // NB: all test users use the same password

	creds := path.Join(t.TempDir(), "creds")
	require.NoError(t, os.WriteFile(creds, []byte(credentialedURL.String()), 0o600))

	originalCredentialHelper, _, err := git.NewCommandContextNoGlobals(git.DefaultContext, "config").AddOptionValues("--global", "credential.helper").RunStdString(&git.RunOpts{})
	if err != nil && !err.IsExitCode(1) {
		// ignore the 'error' thrown when credential.helper is unset (when git config returns 1)
		// but catch all others
		require.NoError(t, err)
	}
	hasOriginalCredentialHelper := (err == nil)

	_, _, err = git.NewCommandContextNoGlobals(git.DefaultContext, "config").AddOptionValues("--global", "credential.helper", fmt.Sprintf("store --file=%s", creds)).RunStdString(&git.RunOpts{})
	require.NoError(t, err)

	defer (func() {
		// reset
		if hasOriginalCredentialHelper {
			_, _, err = git.NewCommandContextNoGlobals(git.DefaultContext, "config").AddOptionValues("--global").AddArguments("credential.helper").AddDynamicArguments(originalCredentialHelper).RunStdString(&git.RunOpts{})
		} else {
			_, _, err = git.NewCommandContextNoGlobals(git.DefaultContext, "config").AddOptionValues("--global").AddOptionValues("--unset").AddArguments("credential.helper").RunStdString(&git.RunOpts{})
		}
		require.NoError(t, err)
	})()

	callback()
}
