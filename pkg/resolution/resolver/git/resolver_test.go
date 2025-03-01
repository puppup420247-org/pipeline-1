/*
Copyright 2022 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package git

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/go-scm/scm/driver/fake"
	"github.com/jenkins-x/go-scm/scm/factory"
	resolverconfig "github.com/tektoncd/pipeline/pkg/apis/config/resolver"
	pipelinev1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"github.com/tektoncd/pipeline/pkg/apis/resolution/v1beta1"
	ttesting "github.com/tektoncd/pipeline/pkg/reconciler/testing"
	resolutioncommon "github.com/tektoncd/pipeline/pkg/resolution/common"
	"github.com/tektoncd/pipeline/pkg/resolution/resolver/framework"
	frtesting "github.com/tektoncd/pipeline/pkg/resolution/resolver/framework/testing"
	"github.com/tektoncd/pipeline/test"
	"github.com/tektoncd/pipeline/test/diff"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/system"

	_ "knative.dev/pkg/system/testing"
)

func TestGetSelector(t *testing.T) {
	resolver := Resolver{}
	sel := resolver.GetSelector(resolverContext())
	if typ, has := sel[resolutioncommon.LabelKeyResolverType]; !has {
		t.Fatalf("unexpected selector: %v", sel)
	} else if typ != labelValueGitResolverType {
		t.Fatalf("unexpected type: %q", typ)
	}
}

func TestValidateParams(t *testing.T) {
	resolver := Resolver{}

	paramsWithRevision := map[string]string{
		urlParam:      "http://foo",
		pathParam:     "bar",
		revisionParam: "baz",
	}

	if err := resolver.ValidateParams(resolverContext(), toParams(paramsWithRevision)); err != nil {
		t.Fatalf("unexpected error validating params: %v", err)
	}
}

func TestValidateParamsNotEnabled(t *testing.T) {
	resolver := Resolver{}

	var err error

	someParams := map[string]string{
		pathParam:     "bar",
		revisionParam: "baz",
	}
	err = resolver.ValidateParams(context.Background(), toParams(someParams))
	if err == nil {
		t.Fatalf("expected disabled err")
	}
	if d := cmp.Diff(disabledError, err.Error()); d != "" {
		t.Errorf("unexpected error: %s", diff.PrintWantGot(d))
	}
}

func TestValidateParams_Failure(t *testing.T) {

	testCases := []struct {
		name        string
		params      map[string]string
		expectedErr string
	}{
		{
			name: "missing multiple",
			params: map[string]string{
				orgParam:  "abcd1234",
				repoParam: "foo",
			},
			expectedErr: fmt.Sprintf("missing required git resolver params: %s, %s", revisionParam, pathParam),
		}, {
			name: "no repo or url",
			params: map[string]string{
				revisionParam: "abcd1234",
				pathParam:     "/foo/bar",
			},
			expectedErr: "must specify one of 'url' or 'repo'",
		}, {
			name: "both repo and url",
			params: map[string]string{
				revisionParam: "abcd1234",
				pathParam:     "/foo/bar",
				urlParam:      "http://foo",
				repoParam:     "foo",
			},
			expectedErr: "cannot specify both 'url' and 'repo'",
		}, {
			name: "no org with repo",
			params: map[string]string{
				revisionParam: "abcd1234",
				pathParam:     "/foo/bar",
				repoParam:     "foo",
			},
			expectedErr: "'org' is required when 'repo' is specified",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &Resolver{}
			err := resolver.ValidateParams(resolverContext(), toParams(tc.params))
			if err == nil {
				t.Fatalf("got no error, but expected: %s", tc.expectedErr)
			}
			if d := cmp.Diff(tc.expectedErr, err.Error()); d != "" {
				t.Errorf("error did not match: %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestGetResolutionTimeoutDefault(t *testing.T) {
	resolver := Resolver{}
	defaultTimeout := 30 * time.Minute
	timeout := resolver.GetResolutionTimeout(resolverContext(), defaultTimeout)
	if timeout != defaultTimeout {
		t.Fatalf("expected default timeout to be returned")
	}
}

func TestGetResolutionTimeoutCustom(t *testing.T) {
	resolver := Resolver{}
	defaultTimeout := 30 * time.Minute
	configTimeout := 5 * time.Second
	config := map[string]string{
		defaultTimeoutKey: configTimeout.String(),
	}
	ctx := framework.InjectResolverConfigToContext(resolverContext(), config)
	timeout := resolver.GetResolutionTimeout(ctx, defaultTimeout)
	if timeout != configTimeout {
		t.Fatalf("expected timeout from config to be returned")
	}
}

func TestResolveNotEnabled(t *testing.T) {
	resolver := Resolver{}

	var err error

	someParams := map[string]string{
		pathParam:     "bar",
		revisionParam: "baz",
	}
	_, err = resolver.Resolve(context.Background(), toParams(someParams))
	if err == nil {
		t.Fatalf("expected disabled err")
	}
	if d := cmp.Diff(disabledError, err.Error()); d != "" {
		t.Errorf("unexpected error: %s", diff.PrintWantGot(d))
	}
}

func TestResolve(t *testing.T) {
	withTemporaryGitConfig(t)

	testOrg := "test-org"
	testRepo := "test-repo"

	refsDir := filepath.Join("testdata", "test-org", "test-repo", "refs")
	mainPipelineYAML, err := ioutil.ReadFile(filepath.Join(refsDir, "main", "pipelines", "example-pipeline.yaml"))
	if err != nil {
		t.Fatalf("couldn't read main pipeline: %v", err)
	}
	otherPipelineYAML, err := ioutil.ReadFile(filepath.Join(refsDir, "other", "pipelines", "example-pipeline.yaml"))
	if err != nil {
		t.Fatalf("couldn't read other pipeline: %v", err)
	}

	mainTaskYAML, err := ioutil.ReadFile(filepath.Join(refsDir, "main", "tasks", "example-task.yaml"))
	if err != nil {
		t.Fatalf("couldn't read main task: %v", err)
	}

	testCases := []struct {
		name           string
		commits        []commitForRepo
		revision       string
		org            string
		repo           string
		useNthCommit   int
		specificCommit string
		pathInRepo     string
		config         map[string]string
		apiToken       string
		expectedStatus *v1beta1.ResolutionRequestStatus
		expectedErr    error
	}{
		{
			name: "clone: single commit with default revision",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			pathInRepo:     "foo/bar/somefile",
			expectedStatus: createStatus([]byte("some content")),
		}, {
			name: "clone: branch revision",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
				Branch:   "other-revision",
			}, {
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "wrong content",
			}},
			revision:       "other-revision",
			pathInRepo:     "foo/bar/somefile",
			expectedStatus: createStatus([]byte("some content")),
		}, {
			name: "clone: commit revision",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}, {
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "different content",
			}},
			pathInRepo:     "foo/bar/somefile",
			useNthCommit:   1,
			expectedStatus: createStatus([]byte("different content")),
		}, {
			name: "clone: tag revision",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
				Tag:      "tag1",
			}, {
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "different content",
			}},
			pathInRepo:     "foo/bar/somefile",
			revision:       "tag1",
			expectedStatus: createStatus([]byte("some content")),
		}, {
			name: "clone: file does not exist",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			pathInRepo:  "foo/bar/some other file",
			expectedErr: createError(`error opening file "foo/bar/some other file": file does not exist`),
		}, {
			name: "clone: revision does not exist",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			revision:    "does-not-exist",
			pathInRepo:  "foo/bar/some other file",
			expectedErr: createError("revision error: reference not found"),
		}, {
			name:       "api: successful task",
			revision:   "main",
			pathInRepo: "tasks/example-task.yaml",
			org:        testOrg,
			repo:       testRepo,
			config: map[string]string{
				ServerURLKey:          "fake",
				SCMTypeKey:            "fake",
				APISecretNameKey:      "token-secret",
				APISecretKeyKey:       "token",
				APISecretNamespaceKey: system.Namespace(),
			},
			apiToken:       "some-token",
			expectedStatus: createStatus(mainTaskYAML),
		}, {
			name:       "api: successful pipeline",
			revision:   "main",
			pathInRepo: "pipelines/example-pipeline.yaml",
			org:        testOrg,
			repo:       testRepo,
			config: map[string]string{
				ServerURLKey:          "fake",
				SCMTypeKey:            "fake",
				APISecretNameKey:      "token-secret",
				APISecretKeyKey:       "token",
				APISecretNamespaceKey: system.Namespace(),
			},
			apiToken:       "some-token",
			expectedStatus: createStatus(mainPipelineYAML),
		}, {
			name:       "api: successful pipeline with default revision",
			pathInRepo: "pipelines/example-pipeline.yaml",
			org:        testOrg,
			repo:       testRepo,
			config: map[string]string{
				ServerURLKey:          "fake",
				SCMTypeKey:            "fake",
				APISecretNameKey:      "token-secret",
				APISecretKeyKey:       "token",
				APISecretNamespaceKey: system.Namespace(),
				defaultRevisionKey:    "other",
			},
			apiToken:       "some-token",
			expectedStatus: createStatus(otherPipelineYAML),
		}, {
			name:       "api: file does not exist",
			revision:   "main",
			pathInRepo: "pipelines/other-pipeline.yaml",
			org:        testOrg,
			repo:       testRepo,
			config: map[string]string{
				ServerURLKey:          "fake",
				SCMTypeKey:            "fake",
				APISecretNameKey:      "token-secret",
				APISecretKeyKey:       "token",
				APISecretNamespaceKey: system.Namespace(),
			},
			apiToken:       "some-token",
			expectedStatus: createFailureStatus(),
			expectedErr:    createError("couldn't fetch resource content: file testdata/test-org/test-repo/refs/main/pipelines/other-pipeline.yaml does not exist: stat testdata/test-org/test-repo/refs/main/pipelines/other-pipeline.yaml: no such file or directory"),
		}, {
			name:       "api: token not found",
			revision:   "main",
			pathInRepo: "pipelines/example-pipeline.yaml",
			org:        testOrg,
			repo:       testRepo,
			config: map[string]string{
				ServerURLKey:          "fake",
				SCMTypeKey:            "fake",
				APISecretNameKey:      "token-secret",
				APISecretKeyKey:       "token",
				APISecretNamespaceKey: system.Namespace(),
			},
			expectedStatus: createFailureStatus(),
			expectedErr:    createError(fmt.Sprintf("cannot get API token, secret token-secret not found in namespace %s", system.Namespace())),
		}, {
			name:       "api: token secret name not specified",
			revision:   "main",
			pathInRepo: "pipelines/example-pipeline.yaml",
			org:        testOrg,
			repo:       testRepo,
			config: map[string]string{
				ServerURLKey:          "fake",
				SCMTypeKey:            "fake",
				APISecretKeyKey:       "token",
				APISecretNamespaceKey: system.Namespace(),
			},
			apiToken:       "some-token",
			expectedStatus: createFailureStatus(),
			expectedErr:    createError("cannot get API token, required when specifying 'repo' param, 'api-token-secret-name' not specified in config"),
		}, {
			name:       "api: token secret key not specified",
			revision:   "main",
			pathInRepo: "pipelines/example-pipeline.yaml",
			org:        testOrg,
			repo:       testRepo,
			config: map[string]string{
				ServerURLKey:          "fake",
				SCMTypeKey:            "fake",
				APISecretNameKey:      "token-secret",
				APISecretNamespaceKey: system.Namespace(),
			},
			apiToken:       "some-token",
			expectedStatus: createFailureStatus(),
			expectedErr:    createError("cannot get API token, required when specifying 'repo' param, 'api-token-secret-key' not specified in config"),
		}, {
			name:       "api: SCM type not specified",
			revision:   "main",
			pathInRepo: "pipelines/example-pipeline.yaml",
			org:        testOrg,
			repo:       testRepo,
			config: map[string]string{
				APISecretNameKey:      "token-secret",
				APISecretKeyKey:       "token",
				APISecretNamespaceKey: system.Namespace(),
			},
			apiToken:       "some-token",
			expectedStatus: createFailureStatus(),
			expectedErr:    createError("missing or empty scm-type value in configmap"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := ttesting.SetupFakeContext(t)

			fakeClone := fmt.Sprintf("https://fake/%s/%s.git", testOrg, testRepo)
			resolver := &Resolver{
				clientFunc: func(driver string, serverURL string, token string, opts ...factory.ClientOptionFunc) (*scm.Client, error) {
					scmClient, scmData := fake.NewDefault()

					// Currently, we only could care about repositories.
					scmData.Repositories = []*scm.Repository{{
						FullName: fmt.Sprintf("%s/%s", testOrg, testRepo),
						Clone:    fakeClone,
					}}

					return scmClient, nil
				},
			}

			var repoPath string
			var commits map[string][]string

			if len(tc.commits) > 0 {
				repoPath, commits = createTestRepo(t, tc.commits)
			}
			cfg := tc.config
			if cfg == nil {
				cfg = make(map[string]string)
			}
			cfg[defaultTimeoutKey] = "1m"
			if cfg[defaultRevisionKey] == "" {
				cfg[defaultRevisionKey] = plumbing.Master.Short()
			}

			request := createRequest(repoPath, tc.org, tc.repo, tc.pathInRepo, tc.revision, tc.specificCommit, tc.useNthCommit, commits)

			d := test.Data{
				ConfigMaps: []*corev1.ConfigMap{{
					ObjectMeta: metav1.ObjectMeta{
						Name:      ConfigMapName,
						Namespace: resolverconfig.ResolversNamespace(system.Namespace()),
					},
					Data: cfg,
				}, {
					ObjectMeta: metav1.ObjectMeta{
						Namespace: resolverconfig.ResolversNamespace(system.Namespace()),
						Name:      resolverconfig.GetFeatureFlagsConfigName(),
					},
					Data: map[string]string{
						"enable-git-resolver": "true",
					},
				}},
				ResolutionRequests: []*v1beta1.ResolutionRequest{request},
			}

			var expectedStatus *v1beta1.ResolutionRequestStatus
			if tc.expectedStatus != nil {
				expectedStatus = tc.expectedStatus.DeepCopy()

				if tc.expectedErr == nil {
					reqParams := make(map[string]string)
					for _, p := range request.Spec.Params {
						reqParams[p.Name] = p.Value.StringVal
					}
					if expectedStatus.Annotations == nil {
						expectedStatus.Annotations = make(map[string]string)
					}
					expectedStatus.Annotations[resolutioncommon.AnnotationKeyContentType] = "application/x-yaml"
					switch {
					case tc.useNthCommit > 0:
						expectedStatus.Annotations[AnnotationKeyRevision] = commits[plumbing.Master.Short()][tc.useNthCommit]
					case tc.revision == "" && reqParams[urlParam] != "":
						expectedStatus.Annotations[AnnotationKeyRevision] = plumbing.Master.Short()
					case tc.revision == "":
						expectedStatus.Annotations[AnnotationKeyRevision] = tc.config[defaultRevisionKey]
					default:
						expectedStatus.Annotations[AnnotationKeyRevision] = tc.revision
					}
					expectedStatus.Annotations[AnnotationKeyPath] = reqParams[pathParam]

					if reqParams[urlParam] != "" {
						expectedStatus.Annotations[AnnotationKeyURL] = reqParams[urlParam]
					} else {
						expectedStatus.Annotations[AnnotationKeyOrg] = reqParams[orgParam]
						expectedStatus.Annotations[AnnotationKeyRepo] = reqParams[repoParam]
						expectedStatus.Annotations[AnnotationKeyURL] = fakeClone
					}
				} else {
					expectedStatus.Status.Conditions[0].Message = tc.expectedErr.Error()
				}
			}

			frtesting.RunResolverReconcileTest(ctx, t, d, resolver, request, expectedStatus, tc.expectedErr, func(resolver framework.Resolver, testAssets test.Assets) {
				if tc.config[APISecretNameKey] == "" || tc.config[APISecretNamespaceKey] == "" || tc.config[APISecretKeyKey] == "" || tc.apiToken == "" {
					return
				}
				tokenSecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tc.config[APISecretNameKey],
						Namespace: tc.config[APISecretNamespaceKey],
					},
					Data: map[string][]byte{
						tc.config[APISecretKeyKey]: []byte(base64.StdEncoding.Strict().EncodeToString([]byte(tc.apiToken))),
					},
					Type: corev1.SecretTypeOpaque,
				}

				if _, err := testAssets.Clients.Kube.CoreV1().Secrets(tc.config[APISecretNamespaceKey]).Create(ctx, tokenSecret, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create test token secret: %v", err)
				}
			})
		})
	}
}

// createTestRepo is used to instantiate a local test repository with the desired commits.
func createTestRepo(t *testing.T, commits []commitForRepo) (string, map[string][]string) {
	t.Helper()
	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("getting test worktree: %v", err)
	}
	if worktree == nil {
		t.Fatal("test worktree not created")
	}

	startingHash := writeAndCommitToTestRepo(t, worktree, tempDir, "", "README", []byte("This is a test"))

	hashesByBranch := make(map[string][]string)

	// Iterate over the commits and add them.
	for _, cmt := range commits {
		branch := cmt.Branch
		if branch == "" {
			branch = plumbing.Master.Short()
		}

		// If we're given a revision, check out that revision.
		coOpts := &git.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(branch),
		}

		if _, ok := hashesByBranch[branch]; !ok && branch != plumbing.Master.Short() {
			coOpts.Hash = plumbing.NewHash(startingHash.String())
			coOpts.Create = true
		}

		if err := worktree.Checkout(coOpts); err != nil {
			t.Fatalf("couldn't do checkout of %s: %v", branch, err)
		}

		hash := writeAndCommitToTestRepo(t, worktree, tempDir, cmt.Dir, cmt.Filename, []byte(cmt.Content))

		if _, ok := hashesByBranch[branch]; !ok {
			hashesByBranch[branch] = []string{hash.String()}
		} else {
			hashesByBranch[branch] = append(hashesByBranch[branch], hash.String())
		}

		if cmt.Tag != "" {
			_, err = repo.CreateTag(cmt.Tag, hash, &git.CreateTagOptions{
				Message: cmt.Tag,
				Tagger: &object.Signature{
					Name:  "Someone",
					Email: "someone@example.com",
					When:  time.Now(),
				},
			})
		}
		if err != nil {
			t.Fatalf("couldn't add tag for %s: %v", cmt.Tag, err)
		}
	}

	return tempDir, hashesByBranch
}

// commitForRepo provides the directory, filename, content and revision for a test commit.
type commitForRepo struct {
	Dir      string
	Filename string
	Content  string
	Branch   string
	Tag      string
}

func writeAndCommitToTestRepo(t *testing.T, worktree *git.Worktree, repoDir string, subPath string, filename string, content []byte) plumbing.Hash {
	t.Helper()

	targetDir := repoDir
	if subPath != "" {
		targetDir = filepath.Join(targetDir, subPath)
		fi, err := os.Stat(targetDir)
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetDir, 0700); err != nil {
				t.Fatalf("couldn't create directory %s in worktree: %v", targetDir, err)
			}
		} else if err != nil {
			t.Fatalf("checking if directory %s in worktree exists: %v", targetDir, err)
		}
		if fi != nil && !fi.IsDir() {
			t.Fatalf("%s already exists but is not a directory", targetDir)
		}
	}

	outfile := filepath.Join(targetDir, filename)
	if err := ioutil.WriteFile(outfile, content, 0600); err != nil {
		t.Fatalf("couldn't write content to file %s: %v", outfile, err)
	}

	_, err := worktree.Add(filepath.Join(subPath, filename))
	if err != nil {
		t.Fatalf("couldn't add file %s to git: %v", outfile, err)
	}

	hash, err := worktree.Commit("adding file for test", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Someone",
			Email: "someone@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("couldn't perform commit for test: %v", err)
	}

	return hash
}

// withTemporaryGitConfig resets the .gitconfig for the duration of the test.
func withTemporaryGitConfig(t *testing.T) {
	gitConfigDir := t.TempDir()
	key := "GIT_CONFIG_GLOBAL"
	t.Setenv(key, filepath.Join(gitConfigDir, "config"))
}

func createRequest(repoURL, apiOrg, apiRepo, pathInRepo, revision, specificCommit string, useNthCommit int, commitsByBranch map[string][]string) *v1beta1.ResolutionRequest {
	rr := &v1beta1.ResolutionRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "resolution.tekton.dev/v1beta1",
			Kind:       "ResolutionRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "rr",
			Namespace:         "foo",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels: map[string]string{
				resolutioncommon.LabelKeyResolverType: labelValueGitResolverType,
			},
		},
		Spec: v1beta1.ResolutionRequestSpec{
			Params: []pipelinev1beta1.Param{{
				Name:  pathParam,
				Value: *pipelinev1beta1.NewStructuredValues(pathInRepo),
			}},
		},
	}

	switch {
	case useNthCommit > 0:
		rr.Spec.Params = append(rr.Spec.Params, pipelinev1beta1.Param{
			Name:  revisionParam,
			Value: *pipelinev1beta1.NewStructuredValues(commitsByBranch[plumbing.Master.Short()][useNthCommit]),
		})
	case specificCommit != "":
		rr.Spec.Params = append(rr.Spec.Params, pipelinev1beta1.Param{
			Name:  revisionParam,
			Value: *pipelinev1beta1.NewStructuredValues(hex.EncodeToString([]byte(specificCommit))),
		})
	case revision != "":
		rr.Spec.Params = append(rr.Spec.Params, pipelinev1beta1.Param{
			Name:  revisionParam,
			Value: *pipelinev1beta1.NewStructuredValues(revision),
		})
	}

	if repoURL != "" {
		rr.Spec.Params = append(rr.Spec.Params, pipelinev1beta1.Param{
			Name:  urlParam,
			Value: *pipelinev1beta1.NewStructuredValues(repoURL),
		})
	} else {
		rr.Spec.Params = append(rr.Spec.Params, pipelinev1beta1.Param{
			Name:  repoParam,
			Value: *pipelinev1beta1.NewStructuredValues(apiRepo),
		})
		rr.Spec.Params = append(rr.Spec.Params, pipelinev1beta1.Param{
			Name:  orgParam,
			Value: *pipelinev1beta1.NewStructuredValues(apiOrg),
		})
	}

	return rr
}

func resolverContext() context.Context {
	return frtesting.ContextWithGitResolverEnabled(context.Background())
}

func createStatus(content []byte) *v1beta1.ResolutionRequestStatus {
	return &v1beta1.ResolutionRequestStatus{
		Status: duckv1.Status{},
		ResolutionRequestStatusFields: v1beta1.ResolutionRequestStatusFields{
			Data: base64.StdEncoding.Strict().EncodeToString(content),
		},
	}
}

func createFailureStatus() *v1beta1.ResolutionRequestStatus {
	return &v1beta1.ResolutionRequestStatus{
		Status: duckv1.Status{
			Conditions: duckv1.Conditions{{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionFalse,
				Reason: resolutioncommon.ReasonResolutionFailed,
			}},
		},
	}
}

func createError(msg string) error {
	return &resolutioncommon.ErrorGettingResource{
		ResolverName: gitResolverName,
		Key:          "foo/rr",
		Original:     errors.New(msg),
	}
}

func toParams(m map[string]string) []pipelinev1beta1.Param {
	var params []pipelinev1beta1.Param

	for k, v := range m {
		params = append(params, pipelinev1beta1.Param{
			Name:  k,
			Value: *pipelinev1beta1.NewStructuredValues(v),
		})
	}

	return params
}
