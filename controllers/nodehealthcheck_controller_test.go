package controllers

import (
	"context"
	"fmt"
	"reflect"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"

	"github.com/medik8s/node-healthcheck-operator/api/v1alpha1"
	"github.com/medik8s/node-healthcheck-operator/controllers/resources"
	"github.com/medik8s/node-healthcheck-operator/controllers/utils"
)

const (
	unhealthyConditionDuration = 10 * time.Second
	nodeUnhealthyIn            = 5 * time.Second
)

var _ = Describe("Node Health Check CR", func() {

	Context("Defaults", func() {
		var underTest *v1alpha1.NodeHealthCheck

		BeforeEach(func() {
			underTest = &v1alpha1.NodeHealthCheck{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: v1alpha1.NodeHealthCheckSpec{
					Selector: metav1.LabelSelector{},
					RemediationTemplate: &v1.ObjectReference{
						Kind:      "InfrastructureRemediationTemplate",
						Namespace: "default",
						Name:      "template",
					},
				},
			}
			err := k8sClient.Create(context.Background(), underTest)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			err := k8sClient.Delete(context.Background(), underTest)
			Expect(err).NotTo(HaveOccurred())
		})

		When("creating a resource", func() {
			It("it should have all default values set", func() {
				Expect(underTest.Namespace).To(BeEmpty())
				Expect(underTest.Spec.UnhealthyConditions).To(HaveLen(2))
				Expect(underTest.Spec.UnhealthyConditions[0].Type).To(Equal(v1.NodeReady))
				Expect(underTest.Spec.UnhealthyConditions[0].Status).To(Equal(v1.ConditionFalse))
				Expect(underTest.Spec.UnhealthyConditions[0].Duration).To(Equal(metav1.Duration{Duration: time.Minute * 5}))
				Expect(underTest.Spec.UnhealthyConditions[1].Type).To(Equal(v1.NodeReady))
				Expect(underTest.Spec.UnhealthyConditions[1].Status).To(Equal(v1.ConditionUnknown))
				Expect(underTest.Spec.UnhealthyConditions[1].Duration).To(Equal(metav1.Duration{Duration: time.Minute * 5}))
				Expect(underTest.Spec.MinHealthy.StrVal).To(Equal(intstr.FromString("51%").StrVal))
				Expect(underTest.Spec.Selector.MatchLabels).To(BeEmpty())
				Expect(underTest.Spec.Selector.MatchExpressions).To(BeEmpty())
			})
		})

		When("updating status", func() {
			It("succeeds updating only part of the fields", func() {
				Expect(underTest.Status).ToNot(BeNil())
				Expect(underTest.Status.HealthyNodes).To(BeNil())
				patch := client.MergeFrom(underTest.DeepCopy())
				underTest.Status.HealthyNodes = pointer.Int(1)
				underTest.Status.ObservedNodes = pointer.Int(6)
				err := k8sClient.Status().Patch(context.Background(), underTest, patch)
				Expect(err).NotTo(HaveOccurred())
				Expect(*underTest.Status.HealthyNodes).To(Equal(1))
				Expect(*underTest.Status.ObservedNodes).To(Equal(6))
				Expect(underTest.Status.InFlightRemediations).To(BeNil())
			})
		})

	})

	Context("Validation", func() {
		var underTest *v1alpha1.NodeHealthCheck

		BeforeEach(func() {
			underTest = &v1alpha1.NodeHealthCheck{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: v1alpha1.NodeHealthCheckSpec{
					RemediationTemplate: &v1.ObjectReference{
						Kind:      "InfrastructureRemediationTemplate",
						Namespace: "default",
						Name:      "template",
					},
				},
			}
		})

		AfterEach(func() {
			_ = k8sClient.Delete(context.Background(), underTest)
		})

		When("specifying an external remediation template", func() {
			It("should succeed creation if a template CR doesn't exists", func() {
				err := k8sClient.Create(context.Background(), underTest)
				Expect(err).NotTo(HaveOccurred())
			})
		})
		When("specifying min healthy", func() {
			It("fails creation on percentage > 100%", func() {
				invalidPercentage := intstr.FromString("150%")
				underTest.Spec.MinHealthy = &invalidPercentage
				err := k8sClient.Create(context.Background(), underTest)
				Expect(errors.IsInvalid(err)).To(BeTrue())
			})

			It("fails creation on negative number", func() {
				// This test does not work yet, because the "minimum" validation
				// of kubebuilder does not work for IntOrString.
				// Un-skip this as soon as this is supported.
				// For now negative minHealthy is validated via webhook.
				Skip("Does not work yet")
				invalidInt := intstr.FromInt(-10)
				underTest.Spec.MinHealthy = &invalidInt
				err := k8sClient.Create(context.Background(), underTest)
				Expect(errors.IsInvalid(err)).To(BeTrue())
			})

			It("succeeds creation on percentage between 0%-100%", func() {
				validPercentage := intstr.FromString("30%")
				underTest.Spec.MinHealthy = &validPercentage
				err := k8sClient.Create(context.Background(), underTest)
				Expect(errors.IsInvalid(err)).To(BeFalse())
			})
		})
	})

	createObjects := func(objects ...client.Object) {
		for _, obj := range objects {
			Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		}
	}

	deleteObjects := func(objects ...client.Object) {
		for _, obj := range objects {
			// ignore errors, CRs might be deleted by reconcile
			_ = k8sClient.Delete(context.Background(), obj)
		}
	}

	Context("Reconciliation", func() {
		const (
			unhealthyNodeName = "unhealthy-worker-node-1"
		)
		var (
			underTest *v1alpha1.NodeHealthCheck
			objects   []client.Object
			//Lease params
			leaseName                             = fmt.Sprintf("%s-%s", "node", unhealthyNodeName)
			mockRequeueDurationIfLeaseTaken       = time.Second * 2
			mockDefaultLeaseDuration              = time.Second * 2
			mockLeaseBuffer                       = time.Second
			otherLeaseDurationInSeconds     int32 = 3
		)

		setupObjects := func(unhealthy int, healthy int, unhealthyNow bool) {
			objects = newNodes(unhealthy, healthy, false, unhealthyNow)
			objects = append(objects, underTest)
		}

		BeforeEach(func() {
			underTest = newNodeHealthCheck()
		})

		JustBeforeEach(func() {
			createObjects(objects...)
			// give the reconciler some time
			time.Sleep(2 * time.Second)
			// get updated NHC
			Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
		})

		AfterEach(func() {
			// delete all created objects
			deleteObjects(objects...)

			// delete all remediation CRs
			var remediationKind string
			if underTest.Spec.RemediationTemplate != nil {
				remediationKind = underTest.Spec.RemediationTemplate.Kind
			} else {
				remediationKind = underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Kind
			}
			if remediationKind != "dummyTemplate" {
				cr := newRemediationCR("", underTest)
				crList := &unstructured.UnstructuredList{Object: cr.Object}
				Expect(k8sClient.List(context.Background(), crList)).To(Succeed())
				for _, item := range crList.Items {
					Expect(k8sClient.Delete(context.Background(), &item)).To(Succeed())
				}
			}

			//cleanup lease
			lease := &coordv1.Lease{}
			err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: leaseNs, Name: leaseName}, lease)
			if err == nil {
				err = k8sClient.Delete(context.Background(), lease)
				Expect(err).NotTo(HaveOccurred())
			}

			// let thing settle a bit
			time.Sleep(1 * time.Second)
		})

		testReconcile := func() {

			When("Nodes are candidates for remediation but remediation template is broken", func() {
				BeforeEach(func() {
					setupObjects(1, 2, true)

					if underTest.Spec.RemediationTemplate != nil {
						underTest.Spec.RemediationTemplate.Kind = "dummyTemplate"
					} else {
						underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Kind = "dummyTemplate"
					}
				})

				It("should set corresponding condition", func() {
					Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseDisabled))
					Expect(underTest.Status.Reason).To(
						And(
							ContainSubstring("failed to get"),
							ContainSubstring("dummyTemplate"),
						))
					Expect(underTest.Status.Conditions).To(ContainElement(
						And(
							HaveField("Type", v1alpha1.ConditionTypeDisabled),
							HaveField("Status", metav1.ConditionTrue),
							HaveField("Reason", v1alpha1.ConditionReasonDisabledTemplateNotFound),
						)))
				})
			})

			Context("Machine owners", func() {
				When("Metal3RemediationTemplate is in wrong namespace", func() {

					BeforeEach(func() {
						setupObjects(1, 2, true)

						// set metal3 template
						if underTest.Spec.RemediationTemplate != nil {
							underTest.Spec.RemediationTemplate.Kind = "Metal3RemediationTemplate"
							underTest.Spec.RemediationTemplate.Name = "nok"
							underTest.Spec.RemediationTemplate.Namespace = "default"
						} else {
							underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Kind = "Metal3RemediationTemplate"
							underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Name = "nok"
							underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Namespace = "default"
						}
					})

					It("should be disabled", func() {
						Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseDisabled))
						Expect(underTest.Status.Reason).To(
							ContainSubstring("Metal3RemediationTemplate must be in the openshift-machine-api namespace"),
						)
						Expect(underTest.Status.Conditions).To(ContainElement(
							And(
								HaveField("Type", v1alpha1.ConditionTypeDisabled),
								HaveField("Status", metav1.ConditionTrue),
								HaveField("Reason", v1alpha1.ConditionReasonDisabledTemplateInvalid),
							)))
					})
				})
			})

			When("few nodes are unhealthy and healthy nodes meet min healthy", func() {
				BeforeEach(func() {
					setupObjects(1, 2, false)
				})

				It("create a remediation CR for each unhealthy node and updates status", func() {
					cr := newRemediationCR(unhealthyNodeName, underTest)
					// first call should fail, because the node gets unready in a few seconds only
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(errors.IsNotFound(err)).To(BeTrue())
					// wait until nodes are unhealthy
					time.Sleep(nodeUnhealthyIn)
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					Expect(cr.Object).To(ContainElement(map[string]interface{}{"size": "foo"}))
					Expect(cr.GetOwnerReferences()).
						To(ContainElement(
							And(
								// Kind and API version aren't set on underTest, envtest issue...
								// Controller is empty for HaveField because false is the zero value?
								HaveField("Name", underTest.Name),
								HaveField("UID", underTest.UID),
							),
						))
					Expect(cr.GetAnnotations()[oldRemediationCRAnnotationKey]).To(BeEmpty())

					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
					Expect(*underTest.Status.HealthyNodes).To(Equal(2))
					Expect(*underTest.Status.ObservedNodes).To(Equal(3))
					Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Name).To(Equal(cr.GetName()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Namespace).To(Equal(cr.GetNamespace()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.UID).To(Equal(cr.GetUID()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Started).ToNot(BeNil())
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).To(BeNil())
					Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))
					Expect(underTest.Status.Reason).ToNot(BeEmpty())
					Expect(underTest.Status.Conditions).To(ContainElement(
						And(
							HaveField("Type", v1alpha1.ConditionTypeDisabled),
							HaveField("Status", metav1.ConditionFalse),
							HaveField("Reason", v1alpha1.ConditionReasonEnabled),
						)))

				})

			})

			When("few nodes are unhealthy and healthy nodes below min healthy", func() {
				BeforeEach(func() {
					setupObjects(4, 3, true)
				})

				It("skips remediation - CR is not created, status updated correctly", func() {
					cr := newRemediationCR(unhealthyNodeName, underTest)
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(errors.IsNotFound(err)).To(BeTrue())

					Expect(*underTest.Status.HealthyNodes).To(Equal(3))
					Expect(*underTest.Status.ObservedNodes).To(Equal(7))
					Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
					Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
					Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
					Expect(underTest.Status.Reason).ToNot(BeEmpty())
				})

			})

			When("few nodes become healthy", func() {
				BeforeEach(func() {
					setupObjects(1, 2, true)
					remediationCR := newRemediationCR("healthy-worker-node-2", underTest)
					remediationCROther := newRemediationCR("healthy-worker-node-1", underTest)
					refs := remediationCROther.GetOwnerReferences()
					refs[0].Name = "other"
					remediationCROther.SetOwnerReferences(refs)
					objects = append(objects, remediationCR, remediationCROther)
				})

				It("deletes an existing remediation CR and updates status", func() {
					cr := newRemediationCR("healthy-worker-node-2", underTest)
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(errors.IsNotFound(err)).To(BeTrue())

					// owned by other NHC, should not be deleted
					cr = newRemediationCR("healthy-worker-node-1", underTest)
					err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(err).NotTo(HaveOccurred())

					cr = newRemediationCR(unhealthyNodeName, underTest)
					err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(err).NotTo(HaveOccurred())

					Expect(*underTest.Status.HealthyNodes).To(Equal(2))
					Expect(*underTest.Status.ObservedNodes).To(Equal(3))
					Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Name).To(Equal(cr.GetName()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Namespace).To(Equal(cr.GetNamespace()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.UID).To(Equal(cr.GetUID()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Started).ToNot(BeNil())
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).To(BeNil())
					Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))
					Expect(underTest.Status.Reason).ToNot(BeEmpty())
				})
			})

			When("an old remediation cr exists", func() {
				BeforeEach(func() {
					setupObjects(1, 2, true)
				})

				AfterEach(func() {
					fakeTime = nil
				})

				It("an alert flag is set on remediation cr", func() {
					By("faking time and triggering another reconcile")
					afterTimeout := time.Now().Add(remediationCRAlertTimeout).Add(2 * time.Minute)
					fakeTime = &afterTimeout
					labels := underTest.Labels
					if labels == nil {
						labels = make(map[string]string)
					}
					labels["trigger"] = "now"
					underTest.Labels = labels
					Expect(k8sClient.Update(context.Background(), underTest)).To(Succeed())
					time.Sleep(2 * time.Second)

					cr := newRemediationCR(unhealthyNodeName, underTest)
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(err).NotTo(HaveOccurred())
					Expect(cr.GetAnnotations()[oldRemediationCRAnnotationKey]).To(Equal("flagon"))
				})
			})

			When("a remediation cr not owned by current NHC exists", func() {
				BeforeEach(func() {
					cr := newRemediationCR(unhealthyNodeName, underTest)
					owners := cr.GetOwnerReferences()
					owners[0].Name = "not-me"
					cr.SetOwnerReferences(owners)
					Expect(k8sClient.Create(context.Background(), cr)).To(Succeed())
					setupObjects(1, 2, true)
				})

				It("remediation cr should not be processed", func() {
					Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
					Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
				})
			})
		}

		Context("with spec.remediationTemplate", func() {
			testReconcile()

			Context("Node Lease", func() {

				BeforeEach(func() {
					setupObjects(1, 2, true)
				})
				When("un unhealthy node becomes healthy", func() {
					It("node lease is removed", func() {
						cr := newRemediationCR(unhealthyNodeName, underTest)
						err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						Expect(err).ToNot(HaveOccurred())
						//Verify lease exist
						lease := &coordv1.Lease{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						Expect(err).ToNot(HaveOccurred())

						//Mock node becoming healthy
						node := &v1.Node{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: unhealthyNodeName}, node)
						Expect(err).ToNot(HaveOccurred())
						for i, c := range node.Status.Conditions {
							if c.Type == v1.NodeReady {
								node.Status.Conditions[i].Status = v1.ConditionTrue
							}
						}
						err = k8sClient.Status().Update(context.Background(), node)
						Expect(err).ToNot(HaveOccurred())

						//Remediation should be removed
						Eventually(
							func() bool {
								err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
								return errors.IsNotFound(err)
							},
							time.Second, time.Millisecond*100).Should(BeTrue())

						//Verify NHC removed the lease
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						Expect(errors.IsNotFound(err)).To(BeTrue())
					})
				})

				When("an unhealthy node lease is already taken", func() {
					BeforeEach(func() {
						mockLeaseParams(mockRequeueDurationIfLeaseTaken, mockDefaultLeaseDuration, mockLeaseBuffer)

						//Create a mock lease that is already taken
						now := metav1.NowMicro()
						lease := &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: leaseNs}, Spec: coordv1.LeaseSpec{HolderIdentity: pointer.String("notNHC"), LeaseDurationSeconds: &otherLeaseDurationInSeconds, RenewTime: &now, AcquireTime: &now}}
						err := k8sClient.Create(context.Background(), lease)
						Expect(err).NotTo(HaveOccurred())
					})

					It("a remediation CR isn't created", func() {
						go debugLeaseLifeCycle(leaseName)
						cr := newRemediationCR(unhealthyNodeName, underTest)
						err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						Expect(errors.IsNotFound(err)).To(BeTrue())

						Expect(*underTest.Status.HealthyNodes).To(Equal(2))
						Expect(*underTest.Status.ObservedNodes).To(Equal(3))
						Expect(underTest.Status.InFlightRemediations).To(HaveLen(0))
						Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
						Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
						Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(0))

						Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
						Expect(underTest.Status.Reason).ToNot(BeEmpty())
						Expect(underTest.Status.Conditions).To(ContainElement(
							And(
								HaveField("Type", v1alpha1.ConditionTypeDisabled),
								HaveField("Status", metav1.ConditionFalse),
								HaveField("Reason", v1alpha1.ConditionReasonEnabled),
							)))
						//debugDelay()
						//expecting NHC to acquire the lease now and create the CR - checking CR first
						Eventually(
							func() error {
								return k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
							},
							mockRequeueDurationIfLeaseTaken+time.Millisecond*100, time.Millisecond*100).ShouldNot(HaveOccurred())

						//Verifying lease is created
						lease := &coordv1.Lease{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						Expect(err).ToNot(HaveOccurred())
						Expect(*lease.Spec.HolderIdentity).To(Equal("NHC"))
						Expect(*lease.Spec.LeaseDurationSeconds).To(Equal(int32(2 + 1 /*2 seconds is DefaultLeaseDuration (mocked) + 1 second buffer (mocked)  */)))
						Expect(lease.Spec.AcquireTime).ToNot(BeNil())
						Expect(*lease.Spec.AcquireTime).To(Equal(*lease.Spec.RenewTime))

						leaseExpireTime := lease.Spec.AcquireTime.Time.Add(mockRequeueDurationIfLeaseTaken*3 + mockLeaseBuffer)
						timeLeftForLease := leaseExpireTime.Sub(time.Now())
						//debugDelay()
						//Wait for lease to be extended
						time.Sleep(timeLeftForLease * 3 / 4)
						lease = &coordv1.Lease{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						//Verify NHC extended the lease
						Expect(err).ToNot(HaveOccurred())
						Expect(*lease.Spec.AcquireTime).ToNot(Equal(*lease.Spec.RenewTime))
						Expect(lease.Spec.RenewTime.Sub(lease.Spec.AcquireTime.Time) > 0).To(BeTrue())

						//Wait for lease to expire
						time.Sleep(timeLeftForLease/4 + time.Millisecond*100)
						lease = &coordv1.Lease{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						//Verify NHC removed the lease
						Expect(errors.IsNotFound(err)).To(BeTrue())

					})

				})
			})

		})

		Context("with a single escalating remediation", func() {

			BeforeEach(func() {
				templateRef := underTest.Spec.RemediationTemplate
				underTest.Spec.RemediationTemplate = nil
				underTest.Spec.EscalatingRemediations = []v1alpha1.EscalatingRemediation{
					{
						RemediationTemplate: *templateRef,
						Order:               0,
						Timeout:             metav1.Duration{Duration: time.Minute},
					},
				}
			})

			testReconcile()
		})

		Context("with multiple escalating remediations", func() {
			longerRemediationTimeout := 5 * time.Second
			shorterRemediationTimeout := 3 * time.Second
			BeforeEach(func() {
				mockLeaseParams(mockRequeueDurationIfLeaseTaken, mockDefaultLeaseDuration, mockLeaseBuffer)

				templateRef1 := underTest.Spec.RemediationTemplate
				underTest.Spec.RemediationTemplate = nil

				templateRef2 := templateRef1.DeepCopy()
				templateRef2.Kind = "Metal3RemediationTemplate"
				templateRef2.Name = "ok"
				templateRef2.Namespace = MachineNamespace

				underTest.Spec.EscalatingRemediations = []v1alpha1.EscalatingRemediation{
					{
						RemediationTemplate: *templateRef1,
						Order:               0,
						Timeout:             metav1.Duration{Duration: longerRemediationTimeout},
					},
					{
						RemediationTemplate: *templateRef2,
						Order:               5,
						Timeout:             metav1.Duration{Duration: 15 * time.Second /*shorterRemediationTimeout*/},
					},
				}

				setupObjects(1, 2, false)

			})

			It("it should try one remediation after another", func() {
				//go debugLeaseLifeCycle(leaseName)
				cr := newRemediationCR(unhealthyNodeName, underTest)
				//TODO mshitrit cleanup
				/*go debugUnstructured(
				func() (*unstructured.Unstructured, error) {
					us := &unstructured.Unstructured{}
					if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), us); err != nil {
						return nil, err
					}
					return us, nil
				})*/
				// first call should fail, because the node gets unready in a few seconds only
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())
				// wait until nodes are unhealthy
				time.Sleep(nodeUnhealthyIn)
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Name).To(Equal(cr.GetName()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Namespace).To(Equal(cr.GetNamespace()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.UID).To(Equal(cr.GetUID()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Started).ToNot(BeNil())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).To(BeNil())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))

				//Verify lease is created
				lease := &coordv1.Lease{}
				err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
				Expect(err).ToNot(HaveOccurred())
				Expect(*lease.Spec.LeaseDurationSeconds).To(Equal(int32(5 + mockLeaseBuffer.Seconds()) /*First escalation timeout (5) + buffer (1) */))
				Expect(lease.Spec.AcquireTime).ToNot(BeNil())
				Expect(*lease.Spec.AcquireTime).To(Equal(*lease.Spec.RenewTime))

				// Wait for 1st remediation to time out and 2nd to start
				time.Sleep(5 * time.Second)

				// get updated CR
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
				Expect(cr.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).ToNot(BeNil())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))

				// get new CR
				cr = newRemediationCRForSecondRemediation(unhealthyNodeName, underTest)
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(2))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.Name).To(Equal(cr.GetName()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.Namespace).To(Equal(cr.GetNamespace()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.UID).To(Equal(cr.GetUID()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Started).ToNot(BeNil())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].TimedOut).To(BeNil())

				//Verify lease was extended
				err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
				Expect(err).ToNot(HaveOccurred())
				//TODO mshitrit replace 15 with const
				Expect(*lease.Spec.LeaseDurationSeconds).To(Equal(int32(15 + mockLeaseBuffer.Seconds()) /*First escalation timeout (15) + buffer (1) */))
				Expect(lease.Spec.AcquireTime).ToNot(BeNil())
				Expect(lease.Spec.RenewTime.Sub(lease.Spec.AcquireTime.Time) > 0).To(BeTrue())

				// Wait for 2nd remediation to time out
				time.Sleep(17 * time.Second)

				// get updated CR
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
				Expect(cr.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].TimedOut).ToNot(BeNil())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))

				//Incorrect lease expire time calculated with short remediation timeout instead of long one
				leaseWrongExpireTimeShort := lease.Spec.AcquireTime.Time.Add(shorterRemediationTimeout*3 + mockLeaseBuffer /*longest remediation timeout (5) multiply by tries (3) and added buffer*/)

				wrongDurationUntilLeaseExpires := leaseWrongExpireTimeShort.Sub(time.Now())
				time.Sleep(wrongDurationUntilLeaseExpires + time.Second)
				//Verify lease still exist (since long expire time wasn't reached)
				err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
				Expect(err).ToNot(HaveOccurred())
				Expect(*lease.Spec.LeaseDurationSeconds).To(Equal(int32(15 + mockLeaseBuffer.Seconds()) /*First escalation timeout (5) + buffer (1) */))
				Expect(lease.Spec.AcquireTime).ToNot(BeNil())

				// make node healthy
				node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: unhealthyNodeName}}
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(node), node)).To(Succeed())
				node.Status.Conditions[0].Status = v1.ConditionTrue
				Expect(k8sClient.Status().Update(context.Background(), node)).To(Succeed())

				//calculating time left for lease
				timeLeftOnLease := time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second - time.Now().Sub(lease.Spec.RenewTime.Time)
				// wait a bit
				time.Sleep(2 * time.Second)
				timeLeftOnLease = timeLeftOnLease - time.Second*2
				//Verify lease has time left before it should expire
				Expect(timeLeftOnLease > time.Millisecond*500).To(BeTrue()) // a bit over 1 second at this stage
				//Verify lease was removed because the CR was deleted (even though there was some time left)
				err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(*underTest.Status.HealthyNodes).To(Equal(3))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(0))
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(0))
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))

			})
		})

		Context("with progressing condition being set", func() {

			BeforeEach(func() {
				templateRef1 := underTest.Spec.RemediationTemplate
				underTest.Spec.RemediationTemplate = nil
				underTest.Spec.EscalatingRemediations = []v1alpha1.EscalatingRemediation{
					{
						RemediationTemplate: *templateRef1,
						Order:               0,
						Timeout:             metav1.Duration{Duration: 5 * time.Minute},
					},
				}
				setupObjects(1, 2, true)
			})

			It("it should timeout early", func() {
				cr := newRemediationCR(unhealthyNodeName, underTest)
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Started).ToNot(BeNil())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).To(BeNil())

				By("letting the remediation stop progressing")
				conditions := []interface{}{
					map[string]interface{}{
						"type":               "Succeeded",
						"status":             "False",
						"lastTransitionTime": time.Now().Format(time.RFC3339),
					},
				}
				unstructured.SetNestedSlice(cr.Object, conditions, "status", "conditions")
				Expect(k8sClient.Status().Update(context.Background(), cr))

				// Wait for hardcoded timeout to expire
				time.Sleep(5 * time.Second)

				// get updated CR
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
				Expect(cr.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).ToNot(BeNil())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))
			})
		})

		Context("control plane nodes", func() {
			When("two control plane nodes are unhealthy, just one should be remediated", func() {
				BeforeEach(func() {
					objects = newNodes(2, 1, true, true)
					objects = append(objects, newNodes(1, 5, false, true)...)
					underTest = newNodeHealthCheck()
					objects = append(objects, underTest)
				})

				It("creates a one remediation CR for control plane node and updates status", func() {
					cr := newRemediationCR("", underTest)
					crList := &unstructured.UnstructuredList{Object: cr.Object}
					Expect(k8sClient.List(context.Background(), crList)).To(Succeed())

					Expect(len(crList.Items)).To(BeNumerically("==", 2), "expected 2 remediations, one for control plane, one for worker")
					Expect(crList.Items).To(ContainElements(
						// the unhealthy worker
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", unhealthyNodeName))),
						// one of the unhealthy control plane nodes
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", ContainSubstring("unhealthy-control-plane-node")))),
					))
					Expect(*underTest.Status.HealthyNodes).To(Equal(6))
					Expect(*underTest.Status.ObservedNodes).To(Equal(9))
					Expect(underTest.Status.InFlightRemediations).To(HaveLen(2))
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(2))
					Expect(underTest.Status.UnhealthyNodes).To(ContainElements(
						And(
							HaveField("Name", unhealthyNodeName),
							HaveField("Remediations", ContainElement(
								And(
									HaveField("Resource.Name", unhealthyNodeName),
									HaveField("Started", Not(BeNil())),
									HaveField("TimedOut", BeNil()),
								),
							)),
						),
						And(
							HaveField("Name", ContainSubstring("unhealthy-control-plane-node")),
							HaveField("Remediations", ContainElement(
								And(
									HaveField("Resource.Name", ContainSubstring("unhealthy-control-plane-node")),
									HaveField("Started", Not(BeNil())),
									HaveField("TimedOut", BeNil()),
								),
							)),
						),
					))
				})
			})
		})

		When("remediation is needed but pauseRequests exists", func() {
			BeforeEach(func() {
				setupObjects(1, 2, true)
				underTest.Spec.PauseRequests = []string{"I'm an admin, asking you to stop remediating this group of nodes"}
			})

			It("skips remediation and updates status", func() {
				cr := newRemediationCR(unhealthyNodeName, underTest)
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
				Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhasePaused))
				Expect(underTest.Status.Reason).ToNot(BeEmpty())
			})
		})

		When("Nodes are candidates for remediation and cluster is upgrading", func() {
			BeforeEach(func() {
				clusterUpgradeRequeueAfter = 5 * time.Second
				upgradeChecker.Upgrading = true
				setupObjects(1, 2, true)
			})

			AfterEach(func() {
				upgradeChecker.Upgrading = false
			})

			It("doesn't not remediate but requeues reconciliation and updates status", func() {
				cr := newRemediationCR(unhealthyNodeName, underTest)
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
				Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
				Expect(underTest.Status.Reason).ToNot(BeEmpty())

				By("stopping upgrade and waiting for requeue")
				upgradeChecker.Upgrading = false
				time.Sleep(10 * time.Second)
				err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(err).ToNot(HaveOccurred())

				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
			})

		})

		Context("Machine owners", func() {
			When("Metal3RemediationTemplate is in correct namespace", func() {

				var machine *machinev1beta1.Machine

				BeforeEach(func() {
					setupObjects(1, 2, true)

					// create machine
					machine = &machinev1beta1.Machine{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-machine",
							Namespace: MachineNamespace,
						},
					}
					objects = append(objects, machine)

					// set machine annotation to unhealthy node
					for _, o := range objects {
						o := o
						if o.GetName() == unhealthyNodeName {
							ann := make(map[string]string)
							ann["machine.openshift.io/machine"] = fmt.Sprintf("%s/%s", machine.Namespace, machine.Name)
							o.SetAnnotations(ann)
						}
					}

					// set metal3 template
					underTest.Spec.RemediationTemplate.Kind = "Metal3RemediationTemplate"
					underTest.Spec.RemediationTemplate.Name = "ok"
					underTest.Spec.RemediationTemplate.Namespace = MachineNamespace

				})

				It("should set owner ref to the machine", func() {
					cr := newRemediationCR(unhealthyNodeName, underTest)
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					Expect(cr.GetOwnerReferences()).To(
						ContainElement(
							And(
								// Kind and API version aren't set on underTest, envtest issue...
								// Controller is empty for HaveField because false is the zero value?
								HaveField("Name", machine.Name),
								HaveField("UID", machine.UID),
							),
						),
					)
				})
			})

		})

	})

	// TODO move to new suite in utils package
	Context("Controller Watches", func() {
		var (
			underTest1 *v1alpha1.NodeHealthCheck
			underTest2 *v1alpha1.NodeHealthCheck
			objects    []client.Object
		)

		JustBeforeEach(func() {
			createObjects(objects...)
			time.Sleep(2 * time.Second)
		})

		AfterEach(func() {
			deleteObjects(objects...)
			time.Sleep(1 * time.Second)
		})

		When("a node changes status and is selectable by one NHC selector", func() {
			BeforeEach(func() {
				objects = newNodes(3, 10, false, true)
				underTest1 = newNodeHealthCheck()
				underTest2 = newNodeHealthCheck()
				underTest2.Name = "test-2"
				emptySelector, _ := metav1.ParseToLabelSelector("fooLabel=bar")
				underTest2.Spec.Selector = *emptySelector
				objects = append(objects, underTest1, underTest2)
			})

			It("creates a reconcile request", func() {
				handler := utils.NHCByNodeMapperFunc(k8sClient, controllerruntime.Log)
				updatedNode := v1.Node{
					ObjectMeta: controllerruntime.ObjectMeta{Name: "healthy-worker-node-1"},
				}
				requests := handler(&updatedNode)
				Expect(len(requests)).To(Equal(1))
				Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: underTest1.GetName()}}))
			})
		})

		When("a node changes status and is selectable by the more 2 NHC selector", func() {
			BeforeEach(func() {
				objects = newNodes(3, 10, false, true)
				underTest1 = newNodeHealthCheck()
				underTest2 = newNodeHealthCheck()
				underTest2.Name = "test-2"
				objects = append(objects, underTest1, underTest2)
			})

			It("creates 2 reconcile requests", func() {
				handler := utils.NHCByNodeMapperFunc(k8sClient, controllerruntime.Log)
				updatedNode := v1.Node{
					ObjectMeta: controllerruntime.ObjectMeta{Name: "healthy-worker-node-1"},
				}
				requests := handler(&updatedNode)
				Expect(len(requests)).To(Equal(2))
				Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: underTest1.GetName()}}))
				Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: underTest2.GetName()}}))
			})
		})
		When("a node changes status and there are no NHC objects", func() {
			BeforeEach(func() {
				objects = newNodes(3, 10, false, true)
			})

			It("doesn't create reconcile requests", func() {
				handler := utils.NHCByNodeMapperFunc(k8sClient, controllerruntime.Log)
				updatedNode := v1.Node{
					ObjectMeta: controllerruntime.ObjectMeta{Name: "healthy-worker-node-1"},
				}
				requests := handler(&updatedNode)
				Expect(requests).To(BeEmpty())
			})
		})
	})

	Context("Node updates", func() {
		var oldConditions []v1.NodeCondition
		var newConditions []v1.NodeCondition

		When("no Ready condition exists on new node", func() {
			BeforeEach(func() {
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
				}
			})
			It("should not request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeFalse())
			})
		})

		When("condition types and statuses equal", func() {
			BeforeEach(func() {
				oldConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
				}
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
				}
			})
			It("should not request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeFalse())
			})
		})

		When("condition type changed", func() {
			BeforeEach(func() {
				oldConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
				}
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
				}
			})
			It("should request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeTrue())
			})
		})

		When("condition status changed", func() {
			BeforeEach(func() {
				oldConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
				}
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionFalse,
					},
				}
			})
			It("should request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeTrue())
			})
		})

		When("condition was added", func() {
			BeforeEach(func() {
				oldConditions = append(newConditions,
					v1.NodeCondition{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
				)
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionFalse,
					},
				}
			})
			It("should request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeTrue())
			})
		})

		When("condition was removed", func() {
			BeforeEach(func() {
				oldConditions = append(newConditions,
					v1.NodeCondition{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
					v1.NodeCondition{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
				)

				newConditions = append(newConditions, v1.NodeCondition{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				})
			})
			It("should request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeTrue())
			})
		})
	})
})

func debugDelay() {
	for i := 0; i < 10; i++ {
		time.Sleep(time.Second)
	}
}

func mockLeaseParams(mockRequeueDurationIfLeaseTaken, mockDefaultLeaseDuration, mockLeaseBuffer time.Duration) {
	orgRequeueIfLeaseTaken := resources.RequeueIfLeaseTaken
	orgDefaultLeaseDuration := resources.DefaultLeaseDuration
	orgLeaseBuffer := resources.LeaseBuffer
	//set up mock values so tests can run in a reasonable time
	resources.RequeueIfLeaseTaken = mockRequeueDurationIfLeaseTaken
	resources.DefaultLeaseDuration = mockDefaultLeaseDuration
	resources.LeaseBuffer = mockLeaseBuffer

	ns := &v1.Namespace{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseNs}, ns); err != nil {
		if errors.IsNotFound(err) {
			ns.Name = leaseNs
			err := k8sClient.Create(context.Background(), ns)
			Expect(err).ToNot(HaveOccurred())
		}

	}

	DeferCleanup(func() {
		resources.RequeueIfLeaseTaken = orgRequeueIfLeaseTaken
		resources.DefaultLeaseDuration = orgDefaultLeaseDuration
		resources.LeaseBuffer = orgLeaseBuffer
	})
}

func newRemediationCR(nodeName string, nhc *v1alpha1.NodeHealthCheck) *unstructured.Unstructured {
	return newRemediationCRImpl(nodeName, nhc, false)
}

func newRemediationCRForSecondRemediation(nodeName string, nhc *v1alpha1.NodeHealthCheck) *unstructured.Unstructured {
	return newRemediationCRImpl(nodeName, nhc, true)
}

func newRemediationCRImpl(nodeName string, nhc *v1alpha1.NodeHealthCheck, use2ndEscRem bool) *unstructured.Unstructured {

	var templateRef v1.ObjectReference
	if nhc.Spec.RemediationTemplate != nil {
		templateRef = *nhc.Spec.RemediationTemplate
	} else {
		templateRef = nhc.Spec.EscalatingRemediations[0].RemediationTemplate
		if use2ndEscRem {
			templateRef = nhc.Spec.EscalatingRemediations[1].RemediationTemplate
		}
	}

	cr := unstructured.Unstructured{}
	cr.SetName(nodeName)
	cr.SetNamespace(templateRef.Namespace)
	kind := templateRef.GroupVersionKind().Kind
	// remove trailing template
	kind = kind[:len(kind)-len("template")]
	cr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   templateRef.GroupVersionKind().Group,
		Version: templateRef.GroupVersionKind().Version,
		Kind:    kind,
	})
	cr.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: nhc.APIVersion,
			Kind:       nhc.Kind,
			Name:       nhc.Name,
			UID:        nhc.UID,
		},
	})
	return &cr
}

func newNodeHealthCheck() *v1alpha1.NodeHealthCheck {
	unhealthy := intstr.FromString("51%")
	return &v1alpha1.NodeHealthCheck{
		TypeMeta: metav1.TypeMeta{
			Kind:       "NodeHealthCheck",
			APIVersion: "remediation.medik8s.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
			UID:  "1234",
		},
		Spec: v1alpha1.NodeHealthCheckSpec{
			Selector:   metav1.LabelSelector{},
			MinHealthy: &unhealthy,
			UnhealthyConditions: []v1alpha1.UnhealthyCondition{
				{
					Type:     v1.NodeReady,
					Status:   v1.ConditionFalse,
					Duration: metav1.Duration{Duration: unhealthyConditionDuration},
				},
			},
			RemediationTemplate: &v1.ObjectReference{
				Kind:       "InfrastructureRemediationTemplate",
				APIVersion: "test.medik8s.io/v1alpha1",
				Namespace:  "default",
				Name:       "template",
			},
		},
	}
}

func newNodes(unhealthy int, healthy int, isControlPlane bool, unhealthyNow bool) []client.Object {
	o := make([]client.Object, 0, healthy+unhealthy)
	roleName := "-worker"
	if isControlPlane {
		roleName = "-control-plane"
	}
	for i := unhealthy; i > 0; i-- {
		node := newNode(fmt.Sprintf("unhealthy%s-node-%d", roleName, i), v1.NodeReady, v1.ConditionFalse, isControlPlane, unhealthyNow)
		o = append(o, node)
	}
	for i := healthy; i > 0; i-- {
		o = append(o, newNode(fmt.Sprintf("healthy%s-node-%d", roleName, i), v1.NodeReady, v1.ConditionTrue, isControlPlane, unhealthyNow))
	}
	return o
}

func newNode(name string, t v1.NodeConditionType, s v1.ConditionStatus, isControlPlane bool, unhealthyNow bool) client.Object {
	labels := make(map[string]string, 1)
	if isControlPlane {
		labels[utils.ControlPlaneRoleLabel] = ""
	} else {
		labels[utils.WorkerRoleLabel] = ""
	}
	// let the node get unhealthy in a few seconds
	transitionTime := time.Now().Add(-(unhealthyConditionDuration - nodeUnhealthyIn + 2*time.Second))
	// unless requested otherwise
	if unhealthyNow {
		transitionTime = time.Now().Add(-(unhealthyConditionDuration + 2*time.Second))
	}
	return &v1.Node{
		TypeMeta: metav1.TypeMeta{Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{
					Type:               t,
					Status:             s,
					LastTransitionTime: metav1.Time{Time: transitionTime},
				},
			},
		},
	}
}

// TODO mshitrit remove
func debugUnstructured(fetch func() (*unstructured.Unstructured, error)) {
	oldLease, currentLease := &unstructured.Unstructured{}, &unstructured.Unstructured{}
	var err error
	count := 0
	isFoundPreviously := true
	for {
		count++
		time.Sleep(time.Millisecond * 100)
		now := time.Now()
		currentLease, err = fetch()
		if err != nil {
			if isFoundPreviously {
				fmt.Println(fmt.Sprintf("####### Element NOT found at %q iteration number: %d #######", now, count))
			} else if count%10 == 0 {
				fmt.Println(fmt.Sprintf("####### Element STILL NOT found at %q iteration number: %d #######", now, count))
			}
			isFoundPreviously = false
		} else if reflect.DeepEqual(currentLease, oldLease) {
			isFoundPreviously = true
			if count%10 == 0 {
				fmt.Println(fmt.Sprintf("####### Element STILL found at %q iteration number: %d , Element:%s  #######", now, count, currentLease))
			}
		} else { //first lease
			oldLease = currentLease.DeepCopy()
			isFoundPreviously = true
			fmt.Println(fmt.Sprintf("####### Element CHANGED at %q iteration number: %d , Element:%s  #######", now, count, currentLease))

		}
	}

}

// TODO mshitrit remove
func debugLeaseLifeCycle(leaseName string) {
	oldLease, currentLease := &coordv1.Lease{}, &coordv1.Lease{}
	count := 0
	isFoundPreviously := true
	for {
		count++
		time.Sleep(time.Millisecond * 100)
		now := time.Now()
		if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, currentLease); err != nil {
			if isFoundPreviously {
				fmt.Println(fmt.Sprintf("####### Lease NOT found at %q iteration number: %d #######", now, count))
			}
			isFoundPreviously = false
		} else if oldLease.Spec.RenewTime == nil { //first lease
			oldLease = currentLease.DeepCopy()
			isFoundPreviously = true
			fmt.Println(fmt.Sprintf("####### Lease found at %q iteration number: %d , AquireTime:%q, Renewtime: %q , LeaseDuration:%d  #######", now, count, currentLease.Spec.AcquireTime, currentLease.Spec.RenewTime, *currentLease.Spec.LeaseDurationSeconds))

		} else if currentLease.Spec.RenewTime.Sub(oldLease.Spec.RenewTime.Time) == 0 {
			isFoundPreviously = true
			if count%10 == 0 {
				fmt.Println(fmt.Sprintf("####### Lease STILL found at %q iteration number: %d , AquireTime:%q, Renewtime: %q , LeaseDuration:%d  #######", now, count, currentLease.Spec.AcquireTime, currentLease.Spec.RenewTime, *currentLease.Spec.LeaseDurationSeconds))
			}
		} else if currentLease.Spec.RenewTime.Sub(oldLease.Spec.RenewTime.Time) > 0 {
			isFoundPreviously = true
			oldLease = currentLease.DeepCopy()
			fmt.Println(fmt.Sprintf("####### Lease RENEWED at %q iteration number: %d , AquireTime:%q, Renewtime: %q , LeaseDuration:%d  #######", now, count, currentLease.Spec.AcquireTime, currentLease.Spec.RenewTime, *currentLease.Spec.LeaseDurationSeconds))
		} else {
			isFoundPreviously = true
			fmt.Println(fmt.Sprintf("####### SHOULDN'T HAPPEN Lease found at %q iteration number: %d , AquireTime:%q, Renewtime: %q , LeaseDuration:%d  #######", now, count, currentLease.Spec.AcquireTime, currentLease.Spec.RenewTime, *currentLease.Spec.LeaseDurationSeconds))
		}

	}

}
