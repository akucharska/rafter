package testsuite

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go"
	"github.com/onsi/gomega"
	"github.com/pkg/errors"
	"k8s.io/client-go/dynamic"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/kyma-project/rafter/pkg/apis/rafter/v1beta1"
	"github.com/kyma-project/rafter/tests/pkg/configmap"
	"github.com/kyma-project/rafter/tests/pkg/file"
	"github.com/kyma-project/rafter/tests/pkg/namespace"
	"github.com/kyma-project/rafter/tests/pkg/upload"
)

type Config struct {
	Namespace         string        `envconfig:"default=test-rafter"`
	BucketName        string        `envconfig:"default=test-bucket"`
	ClusterBucketName string        `envconfig:"default=test-cluster-bucket"`
	CommonAssetPrefix string        `envconfig:"default=test"`
	UploadServiceUrl  string        `envconfig:"default=http://localhost:3000/v1/upload"`
	WaitTimeout       time.Duration `envconfig:"default=30s"`
	Minio             MinioConfig
}

type TestSuite struct {
	namespace     *namespace.Namespace
	configMap     *configmap.Configmap
	bucket        *bucket
	clusterBucket *clusterBucket
	fileUpload    *testData
	asset         *asset
	clusterAsset  *clusterAsset

	t *testing.T
	g *gomega.GomegaWithT

	assetDetails []assetData
	uploadResult *upload.Response

	systemBucketName string
	minioCli         *minio.Client
	cfg              Config

	testId string
}

func New(restConfig *rest.Config, cfg Config, t *testing.T, g *gomega.GomegaWithT) (*TestSuite, error) {
	coreCli, err := corev1.NewForConfig(restConfig)
	if err != nil {
		return nil, errors.Wrap(err, "while creating K8s Core client")
	}

	dynamicCli, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, errors.Wrap(err, "while creating K8s Dynamic client")
	}

	minioCli, err := minio.New(cfg.Minio.Endpoint, cfg.Minio.AccessKey, cfg.Minio.SecretKey, cfg.Minio.UseSSL)
	if err != nil {
		return nil, errors.Wrap(err, "while creating Minio client")
	}

	transCfg := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	minioCli.SetCustomTransport(transCfg)

	ns := namespace.New(coreCli, cfg.Namespace)
	cm := configmap.New(coreCli, cfg.Namespace, cfg.WaitTimeout)

	b := newBucket(dynamicCli, cfg.BucketName, cfg.Namespace, cfg.WaitTimeout, t.Logf)
	cb := newClusterBucket(dynamicCli, cfg.ClusterBucketName, cfg.WaitTimeout, t.Logf)
	a := newAsset(dynamicCli, cfg.Namespace, cfg.BucketName, cfg.WaitTimeout, t.Logf)
	ca := newClusterAsset(dynamicCli, cfg.ClusterBucketName, cfg.WaitTimeout, t.Logf)

	return &TestSuite{
		namespace:     ns,
		configMap:     cm,
		bucket:        b,
		clusterBucket: cb,
		fileUpload:    newTestData(cfg.UploadServiceUrl),
		asset:         a,
		clusterAsset:  ca,
		t:             t,
		g:             g,
		minioCli:      minioCli,
		testId:        "singularity",
		cfg:           cfg,
	}, nil
}

func (t *TestSuite) Run() {

	// clean up leftovers from previous tests
	t.t.Log("Deleting old assets...")
	err := t.asset.DeleteLeftovers(t.testId)
	failOnError(t.g, err)

	t.t.Log("Deleting old cluster assets...")
	err = t.clusterAsset.DeleteLeftovers(t.testId)
	failOnError(t.g, err)

	t.t.Log("Deleting old configmaps...")
	err = t.configMap.DeleteAll(t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Deleting old cluster bucket...")
	err = t.clusterBucket.Delete(t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Deleting old bucket...")
	err = t.bucket.Delete(t.t.Log)
	failOnError(t.g, err)

	// setup environment
	t.t.Log("Creating namespace...")
	err = t.namespace.Create(t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Creating cluster bucket...")
	var resourceVersion string
	resourceVersion, err = t.clusterBucket.Create(t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Waiting for cluster bucket to have ready phase...")
	err = t.clusterBucket.WaitForStatusReady(resourceVersion, t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Creating bucket...")
	resourceVersion, err = t.bucket.Create(t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Waiting for bucket to have ready phase...")
	err = t.bucket.WaitForStatusReady(resourceVersion, t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Uploading test files...")
	uploadResult, err := t.uploadTestFiles()
	failOnError(t.g, err)

	t.t.Log("Uploaded files:\n", uploadResult.UploadedFiles)

	t.uploadResult = uploadResult
	t.systemBucketName = uploadResult.UploadedFiles[0].Bucket

	t.t.Log("Apply test configmap...")
	configMapData, err := t.createConfigmapAssetData()
	failOnError(t.g, err)

	t.t.Log("Preparing metadata...")
	t.assetDetails = convertToAssetResourceDetails(uploadResult, t.cfg.CommonAssetPrefix)
	t.assetDetails = append(t.assetDetails, configMapData)

	t.t.Log("Creating assets...")
	resourceVersion, err = t.asset.CreateMany(t.assetDetails, t.testId, t.t.Log)
	failOnError(t.g, err)
	t.t.Log("Waiting for assets to have ready phase...")
	err = t.asset.WaitForStatusesReady(t.assetDetails, resourceVersion, t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Creating cluster assets...")
	resourceVersion, err = t.clusterAsset.CreateMany(t.assetDetails, t.testId, t.t.Log)
	failOnError(t.g, err)
	t.t.Log("Waiting for cluster assets to have ready phase...")
	err = t.clusterAsset.WaitForStatusesReady(t.assetDetails, resourceVersion, t.t.Log)
	failOnError(t.g, err)

	t.t.Log(fmt.Sprintf("asset details:\n%v", t.assetDetails))
	files, err := t.populateUploadedFiles(t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Verifying uploaded files...")
	err = t.verifyUploadedFiles(files)
	failOnError(t.g, err)

	t.t.Log("Removing assets...")
	err = t.asset.DeleteLeftovers(t.testId, t.t.Log)
	failOnError(t.g, err)

	t.t.Log("Removing cluster assets...")
	err = t.clusterAsset.DeleteLeftovers(t.testId, t.t.Log)
	failOnError(t.g, err)

	err = t.verifyDeletedFiles(files)
	failOnError(t.g, err)
}

func (t *TestSuite) Cleanup() {
	t.t.Log("Cleaning up...")

	err := t.configMap.DeleteAll(t.t.Log)
	failOnError(t.g, err)

	err = t.clusterBucket.Delete(t.t.Log)
	failOnError(t.g, err)

	err = t.bucket.Delete(t.t.Log)
	failOnError(t.g, err)

	err = t.namespace.Delete(t.t.Log)
	failOnError(t.g, err)

	err = deleteFiles(t.minioCli, t.uploadResult, t.t.Logf)
	failOnError(t.g, err)
}

func (t *TestSuite) uploadTestFiles() (*upload.Response, error) {
	uploadResult, err := t.fileUpload.Upload()
	if err != nil {
		return nil, err
	}

	if len(uploadResult.Errors) > 0 {
		return nil, fmt.Errorf("during file upload: %+v", uploadResult.Errors)
	}

	return uploadResult, nil
}

func (t *TestSuite) createConfigmapAssetData() (assetData, error) {
	configmapName := "configmap-test"
	paths := []string{localPath("foo.json"), localPath("bar.png")}

	fir, err := file.Open(paths[0])
	if err != nil {
		return assetData{}, errors.Wrapf(err, "during file reading from path: %s", paths[0])
	}
	defer fir.Close()

	sec, err := file.Open(paths[1])
	if err != nil {
		return assetData{}, errors.Wrapf(err, "during file reading from path: %s", paths[1])
	}
	defer sec.Close()

	t.configMap.Create(configmapName, []*os.File{fir, sec}, t.t.Log)

	return assetData{
		Name: fmt.Sprintf("%s-%s", configmapName, "asset"),
		URL:  fmt.Sprintf("%s/%s", t.cfg.Namespace, configmapName),
		Mode: v1beta1.AssetConfigMap,
	}, nil
}

func (t *TestSuite) populateUploadedFiles(callbacks ...func(...interface{})) ([]uploadedFile, error) {
	var allFiles []uploadedFile
	assetFiles, err := t.asset.PopulateUploadFiles(t.assetDetails, callbacks...)
	if err != nil {
		return nil, err
	}

	t.g.Expect(assetFiles).NotTo(gomega.HaveLen(0))

	allFiles = append(allFiles, assetFiles...)

	clusterAssetFiles, err := t.clusterAsset.PopulateUploadFiles(t.assetDetails)
	if err != nil {
		return nil, err
	}

	t.g.Expect(clusterAssetFiles).NotTo(gomega.HaveLen(0))

	allFiles = append(allFiles, clusterAssetFiles...)

	return allFiles, nil
}

func (t *TestSuite) verifyUploadedFiles(files []uploadedFile) error {
	err := verifyUploadedAssets(files, t.t.Logf)
	if err != nil {
		return errors.Wrap(err, "while verifying uploaded files")
	}
	return nil
}

func (t *TestSuite) verifyDeletedFiles(files []uploadedFile) error {
	err := verifyDeletedAssets(files, t.t.Logf)
	if err != nil {
		return errors.Wrap(err, "while verifying deleted files")
	}
	return nil
}

func failOnError(g *gomega.GomegaWithT, err error) {
	g.Expect(err).NotTo(gomega.HaveOccurred())
}
