package functional_tests

import (
	"context"
	"go/build"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	gstruct "github.com/onsi/gomega/gstruct"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	machinev1 "github.com/openshift/client-go/machine/clientset/versioned"
	. "github.com/project-codeflare/codeflare-common/support"
	"github.com/project-codeflare/instascale/controllers"
	"github.com/project-codeflare/instascale/pkg/config"
	mcadv1beta1 "github.com/project-codeflare/multi-cluster-app-dispatcher/pkg/apis/controller/v1beta1"
	mc "github.com/project-codeflare/multi-cluster-app-dispatcher/pkg/client/clientset/versioned"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	cfg       *rest.Config
	k8sClient client.Client // You'll be using this client in your tests.
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
	err       error
)

func startEnvTest(t *testing.T) *envtest.Environment {
	test := With(t)
	//specify testEnv configuration
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd", "bases"),
			filepath.Join(build.Default.GOPATH, "pkg", "mod", "github.com", "project-codeflare", "multi-cluster-app-dispatcher@v1.38.1", "config", "crd", "bases"),
			filepath.Join(build.Default.GOPATH, "pkg", "mod", "github.com", "openshift", "api@v0.0.0-20220411210816-c3bb724c282a", "machine", "v1beta1"),
		},
	}
	cfg, err = testEnv.Start()
	test.Expect(err).NotTo(HaveOccurred())

	return testEnv
}

func establishClient(t *testing.T) {
	test := With(t)
	err = mcadv1beta1.AddToScheme(scheme.Scheme)
	err = machinev1beta1.AddToScheme(scheme.Scheme)
	test.Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	test.Expect(err).NotTo(HaveOccurred())
	test.Expect(k8sClient).NotTo(BeNil())

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	test.Expect(err).ToNot(HaveOccurred())

	instaScaleController := &controllers.AppWrapperReconciler{
		Client: k8sManager.GetClient(),
		Scheme: k8sManager.GetScheme(),
		Config: config.InstaScaleConfiguration{
			MachineSetsStrategy: "reuse",
			MaxScaleoutAllowed:  5,
		},
	}
	err = instaScaleController.SetupWithManager(context.Background(), k8sManager)
	test.Expect(err).ToNot(HaveOccurred())

	go func() {
		err = k8sManager.Start(ctrl.SetupSignalHandler())
		test.Expect(err).ToNot(HaveOccurred())
	}()

}

func teardownTestEnv(testEnv *envtest.Environment) {
	logger := ctrl.LoggerFrom(ctx)
	if err := testEnv.Stop(); err != nil {
		logger.Error(err, "Error stopping test Environment\n")
	}
}

func instascaleAppwrapper(namespace string) *mcadv1beta1.AppWrapper {
	aw := &mcadv1beta1.AppWrapper{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-instascale",
			Namespace: namespace,
			Labels: map[string]string{
				"orderedinstance": "test.instance1",
			},
			Finalizers: []string{"instascale.codeflare.dev/finalizer"},
		},
		Spec: mcadv1beta1.AppWrapperSpec{
			AggrResources: mcadv1beta1.AppWrapperResourceList{
				GenericItems: []mcadv1beta1.AppWrapperGenericResource{
					{
						DesiredAvailable: 1,
						CustomPodResources: []mcadv1beta1.CustomPodResourceTemplate{
							{
								Replicas: 1,
								Requests: apiv1.ResourceList{
									apiv1.ResourceCPU:    resource.MustParse("250m"),
									apiv1.ResourceMemory: resource.MustParse("512Mi"),
								},
								Limits: apiv1.ResourceList{
									apiv1.ResourceCPU:    resource.MustParse("1"),
									apiv1.ResourceMemory: resource.MustParse("1G"),
								},
							},
						},
					},
				},
			},
		},
	}
	return aw
}

func TestReconciler(t *testing.T) {
	testEnv = startEnvTest(t)      // initiates setting up the EnvTest environment
	defer teardownTestEnv(testEnv) //defer teardown of test environement until function exists

	establishClient(t) // setup client required for test

	test := With(t)

	//create new test namespace
	namespace := test.NewTestNamespace()

	// initializes an appwrapper for the created namespace
	aw := instascaleAppwrapper(namespace.Name)

	// create namespace using envtest client in accordance with created kubeconfig's namespace name
	client, _ := kubernetes.NewForConfig(testEnv.Config)
	client.CoreV1().Namespaces().Create(context.TODO(), &apiv1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace.Name,
		},
		Spec: apiv1.NamespaceSpec{Finalizers: []apiv1.FinalizerName{apiv1.FinalizerName(aw.GetFinalizers()[0])}},
	}, metav1.CreateOptions{})

	//create client to interact with kubernetes machine API
	machineClient, err := machinev1.NewForConfig(cfg)
	test.Expect(err).ToNot(HaveOccurred())

	//read machineset yaml from file `test_instascale_machineset.yml`
	b, err := os.ReadFile("test_instascale_machineset.yml")

	//deserialize kubernetes object
	decode := scheme.Codecs.UniversalDeserializer().Decode
	ms, _, err := decode(b, nil, nil) //decode machineset content of YAML file into kubernetes object
	test.Expect(err).ToNot(HaveOccurred())
	msa := ms.(*machinev1beta1.MachineSet) //asserts that decoded object is of type `*machinev1beta1.MachineSet`

	//create machineset in default namespace
	ms, err = machineClient.MachineV1beta1().MachineSets("default").Create(test.Ctx(), msa, metav1.CreateOptions{})
	test.Expect(err).ToNot(HaveOccurred())

	//assert that the replicas in Machineset specification is 0, before appwrapper creation
	machineset, err := machineClient.MachineV1beta1().MachineSets("default").Get(test.Ctx(), "test-instascale", metav1.GetOptions{})
	test.Expect(machineset.Spec.Replicas).To(gstruct.PointTo(Equal(int32(0))))

	// create MCADclient for interacting with cluster API using cfg provided
	mcadClient, err := mc.NewForConfig(cfg)
	test.Expect(err).ToNot(HaveOccurred())

	// create appwrapper resource using mcadClient
	aw, err = mcadClient.WorkloadV1beta1().AppWrappers(namespace.Name).Create(test.Ctx(), aw, metav1.CreateOptions{})
	test.Expect(err).ToNot(HaveOccurred())
	time.Sleep(10 * time.Second)

	//update appwrapper status to avoid appwrapper dispatch in case of Insufficient resources
	aw.Status = mcadv1beta1.AppWrapperStatus{
		Pending: 1,
		State:   mcadv1beta1.AppWrapperStateEnqueued,
		Conditions: []mcadv1beta1.AppWrapperCondition{
			{
				Type:    mcadv1beta1.AppWrapperCondBackoff,
				Status:  apiv1.ConditionTrue,
				Reason:  "AppWrapperNotRunnable",
				Message: "Insufficient resources to dispatch AppWrapper.",
			},
		},
	}
	_, err = mcadClient.WorkloadV1beta1().AppWrappers(namespace.Name).UpdateStatus(test.Ctx(), aw, metav1.UpdateOptions{})
	test.Expect(err).ToNot(HaveOccurred())
	time.Sleep(10 * time.Second)

	// assert for machine replicas belonging to the machine set after appwrapper creation- there should be 1
	machinesetafter, err := machineClient.MachineV1beta1().MachineSets("default").Get(test.Ctx(), "test-instascale", metav1.GetOptions{})
	test.Expect(err).ToNot(HaveOccurred())
	test.Expect(machinesetafter.Spec.Replicas).To(gstruct.PointTo(Equal(int32(1))))

	// delete appwrapper
	err = mcadClient.WorkloadV1beta1().AppWrappers(namespace.Name).Delete(test.Ctx(), aw.Name, metav1.DeleteOptions{})
	test.Expect(err).ToNot(HaveOccurred())
	time.Sleep(10 * time.Second)

	// assert for machine belonging to the machine set after appwrapper deletion- there should be none
	test.Expect(machineset.Spec.Replicas).To(gstruct.PointTo(Equal(int32(0))))

	// delete machineset
	err = machineClient.MachineV1beta1().MachineSets("default").Delete(test.Ctx(), "test-instascale", metav1.DeleteOptions{})
	test.Expect(err).ToNot(HaveOccurred())

	// delete test namespace
	err = client.CoreV1().Namespaces().Delete(context.TODO(), namespace.Name, metav1.DeleteOptions{})
	test.Expect(err).ToNot(HaveOccurred())
}
