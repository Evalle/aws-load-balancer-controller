package ingress

import (
	"context"
	awssdk "github.com/aws/aws-sdk-go/aws"
	ec2sdk "github.com/aws/aws-sdk-go/service/ec2"
	elbv2sdk "github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/golang/mock/gomock"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/annotations"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/aws/services"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/deploy"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/deploy/elbv2"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/deploy/tracking"
	elbv2model "sigs.k8s.io/aws-load-balancer-controller/pkg/model/elbv2"
	networkingpkg "sigs.k8s.io/aws-load-balancer-controller/pkg/networking"
	testclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"testing"
	"time"
)

func Test_defaultModelBuilder_Build(t *testing.T) {
	type resolveViaDiscoveryCall struct {
		subnets []*ec2sdk.Subnet
		err     error
	}
	type env struct {
		svcs []*corev1.Service
	}
	type listLoadBalancersCall struct {
		matchedLBs  []elbv2.LoadBalancerWithTags
		err			error
	}
	type fields struct {
		resolveViaDiscoveryCalls         []resolveViaDiscoveryCall
		listLoadBalancersCalls	     	 []listLoadBalancersCall
	}
	type args struct {
		ingGroup Group
	}

	ns_1_svc_1 := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns-1",
			Name:      "svc-1",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   32768,
				},
			},
		},
	}
	ns_1_svc_2 := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns-1",
			Name:      "svc-2",
			Annotations: map[string]string{
				"alb.ingress.kubernetes.io/target-type":                  "instance",
				"alb.ingress.kubernetes.io/backend-protocol":             "HTTP",
				"alb.ingress.kubernetes.io/healthcheck-protocol":         "HTTP",
				"alb.ingress.kubernetes.io/healthcheck-port":             "traffic-port",
				"alb.ingress.kubernetes.io/healthcheck-path":             "/",
				"alb.ingress.kubernetes.io/healthcheck-interval-seconds": "15",
				"alb.ingress.kubernetes.io/healthcheck-timeout-seconds":  "5",
				"alb.ingress.kubernetes.io/healthy-threshold-count":      "2",
				"alb.ingress.kubernetes.io/unhealthy-threshold-count":    "2",
				"alb.ingress.kubernetes.io/success-codes":                "200",
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   32768,
				},
			},
		},
	}
	ns_1_svc_3 := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns-1",
			Name:      "svc-3",
			Annotations: map[string]string{
				"alb.ingress.kubernetes.io/target-type":                  "ip",
				"alb.ingress.kubernetes.io/backend-protocol":             "HTTPS",
				"alb.ingress.kubernetes.io/healthcheck-protocol":         "HTTPS",
				"alb.ingress.kubernetes.io/healthcheck-port":             "9090",
				"alb.ingress.kubernetes.io/healthcheck-path":             "/health-check",
				"alb.ingress.kubernetes.io/healthcheck-interval-seconds": "20",
				"alb.ingress.kubernetes.io/healthcheck-timeout-seconds":  "10",
				"alb.ingress.kubernetes.io/healthy-threshold-count":      "7",
				"alb.ingress.kubernetes.io/unhealthy-threshold-count":    "5",
				"alb.ingress.kubernetes.io/success-codes":                "200-300",
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "https",
					Port:       443,
					TargetPort: intstr.FromInt(8443),
					NodePort:   32768,
				},
			},
		},
	}

	resolveViaDiscoveryCallForInternalLB := resolveViaDiscoveryCall{
		subnets: []*ec2sdk.Subnet{
			{
				SubnetId:  awssdk.String("subnet-a"),
				CidrBlock: awssdk.String("192.168.0.0/19"),
			},
			{
				SubnetId:  awssdk.String("subnet-b"),
				CidrBlock: awssdk.String("192.168.32.0/19"),
			},
		},
	}
	resolveViaDiscoveryCallForInternetFacingLB := resolveViaDiscoveryCall{
		subnets: []*ec2sdk.Subnet{
			{
				SubnetId:  awssdk.String("subnet-c"),
				CidrBlock: awssdk.String("192.168.64.0/19"),
			},
			{
				SubnetId:  awssdk.String("subnet-d"),
				CidrBlock: awssdk.String("192.168.96.0/19"),
			},
		},
	}

	listLoadBalancerCallForEmptyLB := listLoadBalancersCall{
		matchedLBs: []elbv2.LoadBalancerWithTags{},
	}

	tests := []struct {
		name          string
		env           env
		args          args
		fields        fields
		wantStackJSON string
		wantErr       error
	}{
		{
			name: "Ingress - vanilla internal",
			env: env{
				svcs: []*corev1.Service{ns_1_svc_1, ns_1_svc_2, ns_1_svc_3},
			},
			fields: fields{
				resolveViaDiscoveryCalls: []resolveViaDiscoveryCall{resolveViaDiscoveryCallForInternalLB},
				listLoadBalancersCalls: []listLoadBalancersCall{listLoadBalancerCallForEmptyLB},
			},
			args: args{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_1.Name,
																ServicePort: intstr.FromString("http"),
															},
														},
														{
															Path: "/svc-2",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_2.Name,
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
										{
											Host: "app-2.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-3",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_3.Name,
																ServicePort: intstr.FromString("https"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantStackJSON: `
{
    "id":"ns-1/ing-1",
    "resources":{
        "AWS::EC2::SecurityGroup":{
            "ManagedLBSecurityGroup":{
                "spec":{
                    "groupName":"k8s-ns1-ing1-bd83176788",
                    "description":"[k8s] Managed SecurityGroup for LoadBalancer",
                    "ingress":[
                        {
                            "ipProtocol":"tcp",
                            "fromPort":80,
                            "toPort":80,
                            "ipRanges":[
                                {
                                    "cidrIP":"0.0.0.0/0"
                                }
                            ]
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::Listener":{
            "80":{
                "spec":{
                    "loadBalancerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::LoadBalancer/LoadBalancer/status/loadBalancerARN"
                    },
                    "port":80,
                    "protocol":"HTTP",
                    "defaultActions":[
                        {
                            "type":"fixed-response",
                            "fixedResponseConfig":{
                                "contentType":"text/plain",
                                "statusCode":"404"
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::ListenerRule":{
            "80:1":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":1,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-1"
                                ]
                            }
                        }
                    ]
                }
            },
            "80:2":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":2,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-2:http/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-2"
                                ]
                            }
                        }
                    ]
                }
            },
            "80:3":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":3,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-3:https/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-2.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-3"
                                ]
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::LoadBalancer":{
            "LoadBalancer":{
                "spec":{
                    "name":"k8s-ns1-ing1-b7e914000d",
                    "type":"application",
                    "scheme":"internal",
                    "ipAddressType":"ipv4",
                    "subnetMapping":[
                        {
                            "subnetID":"subnet-a"
                        },
                        {
                            "subnetID":"subnet-b"
                        }
                    ],
                    "securityGroups":[
                        {
                            "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::TargetGroup":{
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "name":"k8s-ns1-svc1-9889425938",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion":"HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            },
            "ns-1/ing-1-svc-2:http":{
                "spec":{
                    "name":"k8s-ns1-svc2-9889425938",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion":"HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            },
            "ns-1/ing-1-svc-3:https":{
                "spec":{
                    "name":"k8s-ns1-svc3-bf42870fba",
                    "targetType":"ip",
                    "port":8443,
                    "protocol":"HTTPS",
					"protocolVersion":"HTTP1",
                    "healthCheckConfig":{
                        "port":9090,
                        "protocol":"HTTPS",
                        "path":"/health-check",
                        "matcher":{
                            "httpCode":"200-300"
                        },
                        "intervalSeconds":20,
                        "timeoutSeconds":10,
                        "healthyThresholdCount":7,
                        "unhealthyThresholdCount":5
                    }
                }
            }
        },
        "K8S::ElasticLoadBalancingV2::TargetGroupBinding":{
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc1-9889425938",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-1",
                                "port":"http"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            },
            "ns-1/ing-1-svc-2:http":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc2-9889425938",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-2:http/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-2",
                                "port":"http"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            },
            "ns-1/ing-1-svc-3:https":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc3-bf42870fba",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-3:https/status/targetGroupARN"
                            },
                            "targetType":"ip",
                            "serviceRef":{
                                "name":"svc-3",
                                "port":"https"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            }
        }
    }
}`,
		},
		{
			name: "Ingress - vanilla internet-facing",
			env: env{
				svcs: []*corev1.Service{ns_1_svc_1, ns_1_svc_2, ns_1_svc_3},
			},
			fields: fields{
				resolveViaDiscoveryCalls: []resolveViaDiscoveryCall{resolveViaDiscoveryCallForInternetFacingLB},
				listLoadBalancersCalls: []listLoadBalancersCall{listLoadBalancerCallForEmptyLB},
			},
			args: args{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/scheme": "internet-facing",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_1.Name,
																ServicePort: intstr.FromString("http"),
															},
														},
														{
															Path: "/svc-2",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_2.Name,
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
										{
											Host: "app-2.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-3",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_3.Name,
																ServicePort: intstr.FromString("https"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantStackJSON: `
{
    "id":"ns-1/ing-1",
    "resources":{
        "AWS::EC2::SecurityGroup":{
            "ManagedLBSecurityGroup":{
                "spec":{
                    "groupName":"k8s-ns1-ing1-bd83176788",
                    "description":"[k8s] Managed SecurityGroup for LoadBalancer",
                    "ingress":[
                        {
                            "ipProtocol":"tcp",
                            "fromPort":80,
                            "toPort":80,
                            "ipRanges":[
                                {
                                    "cidrIP":"0.0.0.0/0"
                                }
                            ]
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::Listener":{
            "80":{
                "spec":{
                    "loadBalancerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::LoadBalancer/LoadBalancer/status/loadBalancerARN"
                    },
                    "port":80,
                    "protocol":"HTTP",
                    "defaultActions":[
                        {
                            "type":"fixed-response",
                            "fixedResponseConfig":{
                                "contentType":"text/plain",
                                "statusCode":"404"
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::ListenerRule":{
            "80:1":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":1,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-1"
                                ]
                            }
                        }
                    ]
                }
            },
            "80:2":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":2,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-2:http/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-2"
                                ]
                            }
                        }
                    ]
                }
            },
            "80:3":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":3,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-3:https/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-2.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-3"
                                ]
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::LoadBalancer":{
            "LoadBalancer":{
                "spec":{
                    "name":"k8s-ns1-ing1-159dd7a143",
                    "type":"application",
                    "scheme":"internet-facing",
                    "ipAddressType":"ipv4",
                    "subnetMapping":[
                        {
                            "subnetID":"subnet-c"
                        },
                        {
                            "subnetID":"subnet-d"
                        }
                    ],
                    "securityGroups":[
                        {
                            "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::TargetGroup":{
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "name":"k8s-ns1-svc1-9889425938",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion":"HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            },
            "ns-1/ing-1-svc-2:http":{
                "spec":{
                    "name":"k8s-ns1-svc2-9889425938",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion": "HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            },
            "ns-1/ing-1-svc-3:https":{
                "spec":{
                    "name":"k8s-ns1-svc3-bf42870fba",
                    "targetType":"ip",
                    "port":8443,
                    "protocol":"HTTPS",
					"protocolVersion": "HTTP1",
                    "healthCheckConfig":{
                        "port":9090,
                        "protocol":"HTTPS",
                        "path":"/health-check",
                        "matcher":{
                            "httpCode":"200-300"
                        },
                        "intervalSeconds":20,
                        "timeoutSeconds":10,
                        "healthyThresholdCount":7,
                        "unhealthyThresholdCount":5
                    }
                }
            }
        },
        "K8S::ElasticLoadBalancingV2::TargetGroupBinding":{
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc1-9889425938",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-1",
                                "port":"http"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            },
            "ns-1/ing-1-svc-2:http":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc2-9889425938",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-2:http/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-2",
                                "port":"http"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            },
            "ns-1/ing-1-svc-3:https":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc3-bf42870fba",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-3:https/status/targetGroupARN"
                            },
                            "targetType":"ip",
                            "serviceRef":{
                                "name":"svc-3",
                                "port":"https"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            }
        }
    }
}`,
		},
		{
			name: "Ingress - using acm and internet-facing",
			env: env{
				svcs: []*corev1.Service{ns_1_svc_1, ns_1_svc_2, ns_1_svc_3},
			},
			fields: fields{
				resolveViaDiscoveryCalls: []resolveViaDiscoveryCall{resolveViaDiscoveryCallForInternetFacingLB},
				listLoadBalancersCalls: []listLoadBalancersCall{listLoadBalancerCallForEmptyLB},
			},
			args: args{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/scheme":          "internet-facing",
									"alb.ingress.kubernetes.io/certificate-arn": "arn:aws:acm:us-east-1:9999999:certificate/22222222,arn:aws:acm:us-east-1:9999999:certificate/33333333,arn:aws:acm:us-east-1:9999999:certificate/11111111,,arn:aws:acm:us-east-1:9999999:certificate/11111111",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_1.Name,
																ServicePort: intstr.FromString("http"),
															},
														},
														{
															Path: "/svc-2",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_2.Name,
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
										{
											Host: "app-2.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-3",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_3.Name,
																ServicePort: intstr.FromString("https"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantStackJSON: `
{
    "id":"ns-1/ing-1",
    "resources":{
        "AWS::EC2::SecurityGroup":{
            "ManagedLBSecurityGroup":{
                "spec":{
                    "groupName":"k8s-ns1-ing1-bd83176788",
                    "description":"[k8s] Managed SecurityGroup for LoadBalancer",
                    "ingress":[
                        {
                            "ipProtocol":"tcp",
                            "fromPort":443,
                            "toPort":443,
                            "ipRanges":[
                                {
                                    "cidrIP":"0.0.0.0/0"
                                }
                            ]
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::Listener":{
            "443":{
                "spec":{
                    "loadBalancerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::LoadBalancer/LoadBalancer/status/loadBalancerARN"
                    },
					"certificates": [
						{
                        "certificateARN": "arn:aws:acm:us-east-1:9999999:certificate/22222222"
						},
						{
                        "certificateARN": "arn:aws:acm:us-east-1:9999999:certificate/33333333"
						},
						{
                        "certificateARN": "arn:aws:acm:us-east-1:9999999:certificate/11111111"
						}
					],
                    "port":443,
                    "protocol":"HTTPS",
					"sslPolicy":"ELBSecurityPolicy-2016-08",
                    "defaultActions":[
                        {
                            "type":"fixed-response",
                            "fixedResponseConfig":{
                                "contentType":"text/plain",
                                "statusCode":"404"
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::ListenerRule":{
            "443:1":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/443/status/listenerARN"
                    },
                    "priority":1,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-1"
                                ]
                            }
                        }
                    ]
                }
            },
            "443:2":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/443/status/listenerARN"
                    },
                    "priority":2,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-2:http/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-2"
                                ]
                            }
                        }
                    ]
                }
            },
            "443:3":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/443/status/listenerARN"
                    },
                    "priority":3,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-3:https/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-2.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-3"
                                ]
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::LoadBalancer":{
            "LoadBalancer":{
                "spec":{
                    "name":"k8s-ns1-ing1-159dd7a143",
                    "type":"application",
                    "scheme":"internet-facing",
                    "ipAddressType":"ipv4",
                    "subnetMapping":[
                        {
                            "subnetID":"subnet-c"
                        },
                        {
                            "subnetID":"subnet-d"
                        }
                    ],
                    "securityGroups":[
                        {
                            "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::TargetGroup":{
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "name":"k8s-ns1-svc1-9889425938",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion":"HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            },
            "ns-1/ing-1-svc-2:http":{
                "spec":{
                    "name":"k8s-ns1-svc2-9889425938",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion": "HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            },
            "ns-1/ing-1-svc-3:https":{
                "spec":{
                    "name":"k8s-ns1-svc3-bf42870fba",
                    "targetType":"ip",
                    "port":8443,
                    "protocol":"HTTPS",
					"protocolVersion": "HTTP1",
                    "healthCheckConfig":{
                        "port":9090,
                        "protocol":"HTTPS",
                        "path":"/health-check",
                        "matcher":{
                            "httpCode":"200-300"
                        },
                        "intervalSeconds":20,
                        "timeoutSeconds":10,
                        "healthyThresholdCount":7,
                        "unhealthyThresholdCount":5
                    }
                }
            }
        },
        "K8S::ElasticLoadBalancingV2::TargetGroupBinding":{
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc1-9889425938",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-1",
                                "port":"http"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            },
            "ns-1/ing-1-svc-2:http":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc2-9889425938",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-2:http/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-2",
                                "port":"http"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            },
            "ns-1/ing-1-svc-3:https":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc3-bf42870fba",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-3:https/status/targetGroupARN"
                            },
                            "targetType":"ip",
                            "serviceRef":{
                                "name":"svc-3",
                                "port":"https"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            }
        }
    }
}`,
		},
		{
			name: "Ingress - referenced same service port with both name and port",
			env: env{
				svcs: []*corev1.Service{ns_1_svc_1, ns_1_svc_2, ns_1_svc_3},
			},
			fields: fields{
				resolveViaDiscoveryCalls: []resolveViaDiscoveryCall{resolveViaDiscoveryCallForInternalLB},
				listLoadBalancersCalls: []listLoadBalancersCall{listLoadBalancerCallForEmptyLB},
			},
			args: args{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1-name",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_1.Name,
																ServicePort: intstr.FromString("http"),
															},
														},
														{
															Path: "/svc-1-port",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_1.Name,
																ServicePort: intstr.FromInt(80),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantStackJSON: `
{
    "id":"ns-1/ing-1",
    "resources":{
        "AWS::EC2::SecurityGroup":{
            "ManagedLBSecurityGroup":{
                "spec":{
                    "groupName":"k8s-ns1-ing1-bd83176788",
                    "description":"[k8s] Managed SecurityGroup for LoadBalancer",
                    "ingress":[
                        {
                            "ipProtocol":"tcp",
                            "fromPort":80,
                            "toPort":80,
                            "ipRanges":[
                                {
                                    "cidrIP":"0.0.0.0/0"
                                }
                            ]
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::Listener":{
            "80":{
                "spec":{
                    "loadBalancerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::LoadBalancer/LoadBalancer/status/loadBalancerARN"
                    },
                    "port":80,
                    "protocol":"HTTP",
                    "defaultActions":[
                        {
                            "type":"fixed-response",
                            "fixedResponseConfig":{
                                "contentType":"text/plain",
                                "statusCode":"404"
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::ListenerRule":{
            "80:1":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":1,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-1-name"
                                ]
                            }
                        }
                    ]
                }
            },
            "80:2":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":2,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:80/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-1-port"
                                ]
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::LoadBalancer":{
            "LoadBalancer":{
                "spec":{
                    "name":"k8s-ns1-ing1-b7e914000d",
                    "type":"application",
                    "scheme":"internal",
                    "ipAddressType":"ipv4",
                    "subnetMapping":[
                        {
                            "subnetID":"subnet-a"
                        },
                        {
                            "subnetID":"subnet-b"
                        }
                    ],
                    "securityGroups":[
                        {
                            "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::TargetGroup":{
            "ns-1/ing-1-svc-1:80":{
                "spec":{
                    "name":"k8s-ns1-svc1-90b7d93b18",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion": "HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            },
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "name":"k8s-ns1-svc1-9889425938",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion": "HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            }
        },
        "K8S::ElasticLoadBalancingV2::TargetGroupBinding":{
            "ns-1/ing-1-svc-1:80":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc1-90b7d93b18",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:80/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-1",
                                "port":80
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            },
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc1-9889425938",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-1",
                                "port":"http"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            }
        }
    }
}`,
		},
		{
			name: "Ingress - not using subnet auto-discovery and internal",
			env: env{
				svcs: []*corev1.Service{ns_1_svc_1, ns_1_svc_2, ns_1_svc_3},
			},
			fields: fields{
				listLoadBalancersCalls: []listLoadBalancersCall{
					{
						matchedLBs: []elbv2.LoadBalancerWithTags{
							{
								LoadBalancer: &elbv2sdk.LoadBalancer{
									LoadBalancerArn: awssdk.String("lb-1"),
									AvailabilityZones: []*elbv2sdk.AvailabilityZone{
										{
											SubnetId: awssdk.String("subnet-e"),
										},
										{
											SubnetId: awssdk.String("subnet-f"),
										},
									},
								},
								Tags: map[string]string{
									"elbv2.k8s.aws/cluster": "cluster-name",
									"ingress.k8s.aws/stack": "ns-1/ing-1",
								},
							},
							{
								LoadBalancer: &elbv2sdk.LoadBalancer{
									LoadBalancerArn: awssdk.String("lb-2"),
									AvailabilityZones: []*elbv2sdk.AvailabilityZone{
										{
											SubnetId: awssdk.String("subnet-e"),
										},
										{
											SubnetId: awssdk.String("subnet-f"),
										},
									},
								},
								Tags: map[string]string{
									"keyA": "valueA2",
									"keyB": "valueB2",
								},
							},
							{
								LoadBalancer: &elbv2sdk.LoadBalancer{
									LoadBalancerArn: awssdk.String("lb-3"),
									AvailabilityZones: []*elbv2sdk.AvailabilityZone{
										{
											SubnetId: awssdk.String("subnet-e"),
										},
										{
											SubnetId: awssdk.String("subnet-f"),
										},
									},
								},
								Tags: map[string]string{
									"keyA": "valueA3",
									"keyB": "valueB3",
								},
							},
						},
					},
				},
			},
			args: args{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_1.Name,
																ServicePort: intstr.FromString("http"),
															},
														},
														{
															Path: "/svc-2",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_2.Name,
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
										{
											Host: "app-2.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-3",
															Backend: networking.IngressBackend{
																ServiceName: ns_1_svc_3.Name,
																ServicePort: intstr.FromString("https"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantStackJSON: `
{
    "id":"ns-1/ing-1",
    "resources":{
        "AWS::EC2::SecurityGroup":{
            "ManagedLBSecurityGroup":{
                "spec":{
                    "groupName":"k8s-ns1-ing1-bd83176788",
                    "description":"[k8s] Managed SecurityGroup for LoadBalancer",
                    "ingress":[
                        {
                            "ipProtocol":"tcp",
                            "fromPort":80,
                            "toPort":80,
                            "ipRanges":[
                                {
                                    "cidrIP":"0.0.0.0/0"
                                }
                            ]
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::Listener":{
            "80":{
                "spec":{
                    "loadBalancerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::LoadBalancer/LoadBalancer/status/loadBalancerARN"
                    },
                    "port":80,
                    "protocol":"HTTP",
                    "defaultActions":[
                        {
                            "type":"fixed-response",
                            "fixedResponseConfig":{
                                "contentType":"text/plain",
                                "statusCode":"404"
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::ListenerRule":{
            "80:1":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":1,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-1"
                                ]
                            }
                        }
                    ]
                }
            },
            "80:2":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":2,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-2:http/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-1.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-2"
                                ]
                            }
                        }
                    ]
                }
            },
            "80:3":{
                "spec":{
                    "listenerARN":{
                        "$ref":"#/resources/AWS::ElasticLoadBalancingV2::Listener/80/status/listenerARN"
                    },
                    "priority":3,
                    "actions":[
                        {
                            "type":"forward",
                            "forwardConfig":{
                                "targetGroups":[
                                    {
                                        "targetGroupARN":{
                                            "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-3:https/status/targetGroupARN"
                                        }
                                    }
                                ]
                            }
                        }
                    ],
                    "conditions":[
                        {
                            "field":"host-header",
                            "hostHeaderConfig":{
                                "values":[
                                    "app-2.example.com"
                                ]
                            }
                        },
                        {
                            "field":"path-pattern",
                            "pathPatternConfig":{
                                "values":[
                                    "/svc-3"
                                ]
                            }
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::LoadBalancer":{
            "LoadBalancer":{
                "spec":{
                    "name":"k8s-ns1-ing1-b7e914000d",
                    "type":"application",
                    "scheme":"internal",
                    "ipAddressType":"ipv4",
                    "subnetMapping":[
                        {
                            "subnetID":"subnet-e"
                        },
                        {
                            "subnetID":"subnet-f"
                        }
                    ],
                    "securityGroups":[
                        {
                            "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                        }
                    ]
                }
            }
        },
        "AWS::ElasticLoadBalancingV2::TargetGroup":{
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "name":"k8s-ns1-svc1-9889425938",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion":"HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            },
            "ns-1/ing-1-svc-2:http":{
                "spec":{
                    "name":"k8s-ns1-svc2-9889425938",
                    "targetType":"instance",
                    "port":32768,
                    "protocol":"HTTP",
					"protocolVersion":"HTTP1",
                    "healthCheckConfig":{
                        "port":"traffic-port",
                        "protocol":"HTTP",
                        "path":"/",
                        "matcher":{
                            "httpCode":"200"
                        },
                        "intervalSeconds":15,
                        "timeoutSeconds":5,
                        "healthyThresholdCount":2,
                        "unhealthyThresholdCount":2
                    }
                }
            },
            "ns-1/ing-1-svc-3:https":{
                "spec":{
                    "name":"k8s-ns1-svc3-bf42870fba",
                    "targetType":"ip",
                    "port":8443,
                    "protocol":"HTTPS",
					"protocolVersion":"HTTP1",
                    "healthCheckConfig":{
                        "port":9090,
                        "protocol":"HTTPS",
                        "path":"/health-check",
                        "matcher":{
                            "httpCode":"200-300"
                        },
                        "intervalSeconds":20,
                        "timeoutSeconds":10,
                        "healthyThresholdCount":7,
                        "unhealthyThresholdCount":5
                    }
                }
            }
        },
        "K8S::ElasticLoadBalancingV2::TargetGroupBinding":{
            "ns-1/ing-1-svc-1:http":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc1-9889425938",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-1:http/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-1",
                                "port":"http"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            },
            "ns-1/ing-1-svc-2:http":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc2-9889425938",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-2:http/status/targetGroupARN"
                            },
                            "targetType":"instance",
                            "serviceRef":{
                                "name":"svc-2",
                                "port":"http"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            },
            "ns-1/ing-1-svc-3:https":{
                "spec":{
                    "template":{
                        "metadata":{
                            "name":"k8s-ns1-svc3-bf42870fba",
                            "namespace":"ns-1",
                            "creationTimestamp":null
                        },
                        "spec":{
                            "targetGroupARN":{
                                "$ref":"#/resources/AWS::ElasticLoadBalancingV2::TargetGroup/ns-1/ing-1-svc-3:https/status/targetGroupARN"
                            },
                            "targetType":"ip",
                            "serviceRef":{
                                "name":"svc-3",
                                "port":"https"
                            },
                            "networking":{
                                "ingress":[
                                    {
                                        "from":[
                                            {
                                                "securityGroup":{
                                                    "groupID":{
                                                        "$ref":"#/resources/AWS::EC2::SecurityGroup/ManagedLBSecurityGroup/status/groupID"
                                                    }
                                                }
                                            }
                                        ],
                                        "ports":[
                                            {
                                                "protocol":"TCP"
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            }
        }
    }
}`,
		},
		{
			name: "Ingress - deletion protection enabled error",
			env: env{
				svcs: []*corev1.Service{ns_1_svc_1, ns_1_svc_2, ns_1_svc_3},
			},
			args: args{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					InactiveMembers: []*networking.Ingress{
						{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "hello-ingress",
								Annotations: map[string]string{
									"kubernetes.io/ingress.class": "alb",
									"alb.ingress.kubernetes.io/load-balancer-attributes": "deletion_protection.enabled=true",
								},
								Finalizers: []string{
									"ingress.k8s.aws/resources",
								},
								DeletionTimestamp: &metav1.Time{
									Time: time.Now(),
								},
							},
						},
					},
				},
			},
			wantErr: errors.New("deletion_protection is enabled, cannot delete the ingress: hello-ingress"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			elbv2TaggingManager := elbv2.NewMockTaggingManager(ctrl)
			for _, call :=range tt.fields.listLoadBalancersCalls {
				elbv2TaggingManager.EXPECT().ListLoadBalancers(gomock.Any(), gomock.Any()).Return(call.matchedLBs, call.err)
			}

			ctx := context.Background()
			k8sSchema := runtime.NewScheme()
			clientgoscheme.AddToScheme(k8sSchema)
			k8sClient := testclient.NewFakeClientWithScheme(k8sSchema)
			for _, svc := range tt.env.svcs {
				assert.NoError(t, k8sClient.Create(ctx, svc.DeepCopy()))
			}
			eventRecorder := record.NewFakeRecorder(10)
			vpcID := "vpc-dummy"
			clusterName := "cluster-dummy"
			ec2Client := services.NewMockEC2(ctrl)
			subnetsResolver := networkingpkg.NewMockSubnetsResolver(ctrl)
			for _, call := range tt.fields.resolveViaDiscoveryCalls {
				subnetsResolver.EXPECT().ResolveViaDiscovery(gomock.Any(), gomock.Any()).Return(call.subnets, call.err)
			}

			certDiscovery := NewMockCertDiscovery(ctrl)
			annotationParser := annotations.NewSuffixAnnotationParser("alb.ingress.kubernetes.io")
			authConfigBuilder := NewDefaultAuthConfigBuilder(annotationParser)
			enhancedBackendBuilder := NewDefaultEnhancedBackendBuilder(k8sClient, annotationParser, authConfigBuilder)
			ruleOptimizer := NewDefaultRuleOptimizer(&log.NullLogger{})
			trackingProvider := tracking.NewDefaultProvider("ingress.k8s.aws", clusterName)
			stackMarshaller := deploy.NewDefaultStackMarshaller()

			b := &defaultModelBuilder{
				k8sClient:              k8sClient,
				eventRecorder:          eventRecorder,
				ec2Client:              ec2Client,
				vpcID:                  vpcID,
				clusterName:            clusterName,
				annotationParser:       annotationParser,
				subnetsResolver:        subnetsResolver,
				certDiscovery:          certDiscovery,
				authConfigBuilder:      authConfigBuilder,
				enhancedBackendBuilder: enhancedBackendBuilder,
				ruleOptimizer:          ruleOptimizer,
				trackingProvider:       trackingProvider,
				elbv2TaggingManager:    elbv2TaggingManager,
				logger:                 &log.NullLogger{},

				defaultSSLPolicy: "ELBSecurityPolicy-2016-08",
			}

			gotStack, _, err := b.Build(context.Background(), tt.args.ingGroup)
			if tt.wantErr != nil {
				assert.EqualError(t, err, tt.wantErr.Error())
			} else {
				assert.NoError(t, err)
				stackJSON, err := stackMarshaller.Marshal(gotStack)
				assert.NoError(t, err)
				assert.JSONEq(t, tt.wantStackJSON, stackJSON)
			}
		})
	}
}

func Test_defaultModelBuildTask_buildSSLRedirectConfig(t *testing.T) {
	type fields struct {
		ingGroup Group
	}
	type args struct {
		listenPortConfigByPort map[int64]listenPortConfig
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    *SSLRedirectConfig
		wantErr error
	}{
		{
			name: "single Ingress without ssl-redirect annotation",
			fields: fields{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: "svc-1",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			args: args{
				listenPortConfigByPort: map[int64]listenPortConfig{
					80: {
						protocol: elbv2model.ProtocolHTTP,
					},
					443: {
						protocol: elbv2model.ProtocolHTTPS,
					},
				},
			},
			want:    nil,
			wantErr: nil,
		},
		{
			name: "single Ingress with ssl-redirect annotation",
			fields: fields{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/ssl-redirect": "443",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: "svc-1",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			args: args{
				listenPortConfigByPort: map[int64]listenPortConfig{
					80: {
						protocol: elbv2model.ProtocolHTTP,
					},
					443: {
						protocol: elbv2model.ProtocolHTTPS,
					},
				},
			},
			want: &SSLRedirectConfig{
				SSLPort:    443,
				StatusCode: "HTTP_301",
			},
			wantErr: nil,
		},
		{
			name: "single Ingress with ssl-redirect annotation but refer non-exists port",
			fields: fields{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/ssl-redirect": "8443",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: "svc-1",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			args: args{
				listenPortConfigByPort: map[int64]listenPortConfig{
					80: {
						protocol: elbv2model.ProtocolHTTP,
					},
					443: {
						protocol: elbv2model.ProtocolHTTPS,
					},
				},
			},
			want:    nil,
			wantErr: errors.New("listener does not exist for SSLRedirect port: 8443"),
		},
		{
			name: "single Ingress with ssl-redirect annotation but refer non-SSL port",
			fields: fields{
				ingGroup: Group{
					ID: GroupID{Namespace: "ns-1", Name: "ing-1"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/ssl-redirect": "80",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: "svc-1",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			args: args{
				listenPortConfigByPort: map[int64]listenPortConfig{
					80: {
						protocol: elbv2model.ProtocolHTTP,
					},
					443: {
						protocol: elbv2model.ProtocolHTTPS,
					},
				},
			},
			want:    nil,
			wantErr: errors.New("listener protocol non-SSL for SSLRedirect port: 80"),
		},
		{
			name: "multiple Ingress without ssl-redirect annotation",
			fields: fields{
				ingGroup: Group{
					ID: GroupID{Namespace: "", Name: "awesome-group"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: "svc-1",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-2",
								Name:      "ing-2",
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-2.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-2",
															Backend: networking.IngressBackend{
																ServiceName: "svc-2",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			args: args{
				listenPortConfigByPort: map[int64]listenPortConfig{
					80: {
						protocol: elbv2model.ProtocolHTTP,
					},
					443: {
						protocol: elbv2model.ProtocolHTTPS,
					},
				},
			},
			want:    nil,
			wantErr: nil,
		},
		{
			name: "multiple Ingress with one ingress have ssl-redirect annotation",
			fields: fields{
				ingGroup: Group{
					ID: GroupID{Namespace: "", Name: "awesome-group"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/ssl-redirect": "443",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: "svc-1",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-2",
								Name:      "ing-2",
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-2.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-2",
															Backend: networking.IngressBackend{
																ServiceName: "svc-2",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			args: args{
				listenPortConfigByPort: map[int64]listenPortConfig{
					80: {
						protocol: elbv2model.ProtocolHTTP,
					},
					443: {
						protocol: elbv2model.ProtocolHTTPS,
					},
				},
			},
			want: &SSLRedirectConfig{
				SSLPort:    443,
				StatusCode: "HTTP_301",
			},
			wantErr: nil,
		},
		{
			name: "multiple Ingress with same ssl-redirect annotation",
			fields: fields{
				ingGroup: Group{
					ID: GroupID{Namespace: "", Name: "awesome-group"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/ssl-redirect": "443",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: "svc-1",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-2",
								Name:      "ing-2",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/ssl-redirect": "443",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-2.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-2",
															Backend: networking.IngressBackend{
																ServiceName: "svc-2",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			args: args{
				listenPortConfigByPort: map[int64]listenPortConfig{
					80: {
						protocol: elbv2model.ProtocolHTTP,
					},
					443: {
						protocol: elbv2model.ProtocolHTTPS,
					},
				},
			},
			want: &SSLRedirectConfig{
				SSLPort:    443,
				StatusCode: "HTTP_301",
			},
			wantErr: nil,
		},
		{
			name: "multiple Ingress with conflicting ssl-redirect annotation",
			fields: fields{
				ingGroup: Group{
					ID: GroupID{Namespace: "", Name: "awesome-group"},
					Members: []ClassifiedIngress{
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-1",
								Name:      "ing-1",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/ssl-redirect": "443",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-1.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-1",
															Backend: networking.IngressBackend{
																ServiceName: "svc-1",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
						{
							Ing: &networking.Ingress{ObjectMeta: metav1.ObjectMeta{
								Namespace: "ns-2",
								Name:      "ing-2",
								Annotations: map[string]string{
									"alb.ingress.kubernetes.io/ssl-redirect": "8443",
								},
							},
								Spec: networking.IngressSpec{
									Rules: []networking.IngressRule{
										{
											Host: "app-2.example.com",
											IngressRuleValue: networking.IngressRuleValue{
												HTTP: &networking.HTTPIngressRuleValue{
													Paths: []networking.HTTPIngressPath{
														{
															Path: "/svc-2",
															Backend: networking.IngressBackend{
																ServiceName: "svc-2",
																ServicePort: intstr.FromString("http"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			args: args{
				listenPortConfigByPort: map[int64]listenPortConfig{
					80: {
						protocol: elbv2model.ProtocolHTTP,
					},
					443: {
						protocol: elbv2model.ProtocolHTTPS,
					},
				},
			},
			want:    nil,
			wantErr: errors.New("conflicting sslRedirect port: [443 8443]"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotationParser := annotations.NewSuffixAnnotationParser("alb.ingress.kubernetes.io")
			task := &defaultModelBuildTask{
				annotationParser: annotationParser,
				ingGroup:         tt.fields.ingGroup,
			}
			got, err := task.buildSSLRedirectConfig(context.Background(), tt.args.listenPortConfigByPort)
			if tt.wantErr != nil {
				assert.EqualError(t, err, tt.wantErr.Error())
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
