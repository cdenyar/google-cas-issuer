package issuers

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io/ioutil"
	"os"
	"text/template"
	"time"

	"filippo.io/age"
	certmanagerv1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	cmmetav1 "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/jetstack/google-cas-issuer/test/e2e/framework"
	"github.com/jetstack/google-cas-issuer/test/e2e/framework/config"
	"github.com/jetstack/google-cas-issuer/test/e2e/util"
)

const (
	issuerYAML string = `apiVersion: cas-issuer.jetstack.io/v1beta1
kind: GoogleCASIssuer
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  project: {{ .Project }}
  location: {{ .Location }}
  caPoolId: {{ .Pool }}
  credentials:
    name: {{ .SecretName }}
    key: "{{ .SecretKey }}"
`

	clusterIssuerYaml string = `apiVersion: cas-issuer.jetstack.io/v1beta1
kind: GoogleCASClusterIssuer
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  project: {{ .Project }}
  location: {{ .Location }}
  caPoolId: {{ .Pool }}
  credentials
    name: {{ .SecretName }}
    key: "{{ .SecretKey }}"`
)

//go:embed google-sa
var googleSa []byte

type templateConfig struct {
	Name       string
	Namespace  string
	Project    string
	Location   string
	Pool       string
	SecretName string
	SecretKey  string
}

var _ = framework.CasesDescribe("issuers", func() {
	f := framework.NewDefaultFramework("issuer")
	cfg := config.GetConfig()
	It("Tests Issuer functionality", func() {
		By("Decrypting issuer secret")
		encSecret := bytes.NewReader(googleSa)
		identity, err := age.ParseX25519Identity(os.Getenv("AGE_SECRET_KEY"))
		Expect(err).NotTo(HaveOccurred())
		decSecret, err := age.Decrypt(encSecret, identity)
		Expect(err).NotTo(HaveOccurred())

		By("Creating a secret for the issuer")
		secretName := "casissuer-" + util.RandomString(5)
		secretData := make(map[string][]byte)
		data, err := ioutil.ReadAll(decSecret)
		Expect(err).NotTo(HaveOccurred())
		secretData["google.json"] = data
		Expect(err).NotTo(HaveOccurred())
		_, err = f.KubeClientSet.CoreV1().Secrets(cfg.Namespace).Create(
			context.TODO(),
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: cfg.Namespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: secretData,
			},
			metav1.CreateOptions{},
		)
		Expect(err).NotTo(HaveOccurred())

		By("Constructing a random issuer")
		t := &templateConfig{
			Name:       "issuer-" + util.RandomString(5),
			Namespace:  cfg.Namespace,
			Project:    cfg.Project,
			Location:   cfg.Location,
			Pool:       cfg.CaPoolId,
			SecretName: secretName,
			SecretKey:  "google.json",
		}
		buf := &bytes.Buffer{}
		err = template.Must(template.New("issuer").Parse(issuerYAML)).Execute(buf, t)
		Expect(err).NotTo(HaveOccurred())

		By("Creating dynamic object")
		dec := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
		apiObject := &unstructured.Unstructured{}
		_, gvk, err := dec.Decode(buf.Bytes(), nil, apiObject)
		Expect(err).NotTo(HaveOccurred())
		mapping, err := f.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		Expect(err).NotTo(HaveOccurred())

		dr := f.DynamicClientSet.Resource(mapping.Resource).Namespace(apiObject.GetNamespace())

		By("Creating issuer " + t.Namespace + "/" + t.Name)
		_, err = dr.Create(context.TODO(), apiObject, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for issuer to become ready")
		err = wait.PollImmediate(time.Second, time.Second*10, func() (done bool, err error) {
			newObj, err := dr.Get(context.TODO(), t.Name, metav1.GetOptions{})
			if err != nil {
				return true, err
			}
			status, found, err := unstructured.NestedMap(newObj.Object, "status")
			if err != nil {
				return false, err
			}
			if !found {
				return false, nil
			}
			conditions, ok := status["conditions"].([]interface{})
			if !ok {
				return false, errors.New(".status.conditions is not []interface{}")
			}
			for _, c := range conditions {
				cond, ok := c.(map[string]interface{})
				if !ok {
					return false, errors.New(".status.conditions doesn't contain a map")
				}
				if cond["type"].(string) != "Ready" {
					continue
				}
				if cond["status"].(string) == "True" {
					return true, nil
				} else {
					reasonMessage := "Issuer is not ready: "
					reason, found := cond["reason"]
					if found {
						reasonMessage = reasonMessage + " " + reason.(string)
					}
					message, found := cond["message"]
					if found {
						reasonMessage = reasonMessage + " " + message.(string)
					}
					return false, errors.New(reasonMessage)
				}
			}
			return false, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Creating a certificate")
		certName := "casissuer-e2e-" + util.RandomString(5)
		_, err = f.CMClientSet.CertmanagerV1().Certificates(cfg.Namespace).Create(context.TODO(), &certmanagerv1.Certificate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      certName,
				Namespace: cfg.Namespace,
			},
			Spec: certmanagerv1.CertificateSpec{
				SecretName:  certName,
				CommonName:  certName,
				DNSNames:    []string{certName, "e2etests.invalid"},
				Duration:    &metav1.Duration{Duration: 24 * time.Hour},
				RenewBefore: &metav1.Duration{Duration: 8 * time.Hour},
				IssuerRef: cmmetav1.ObjectReference{
					Name:  t.Name,
					Kind:  gvk.Kind,
					Group: gvk.Group,
				},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		By("Waiting for certificate to become ready")
		_, err = f.Helper().WaitForCertificateReady(cfg.Namespace, certName, 10*time.Second)
		Expect(err).NotTo(HaveOccurred())
	})
})
