package platformcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func idConfigMap(t *testing.T, c client.Client, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "global-vpc-system", Name: name}, cm); err != nil {
		t.Fatal(err)
	}
	return cm
}

func otherShardKey(key string) string {
	for n := 0; ; n++ {
		candidate := fmt.Sprintf("future/%d", n)
		if networkIDShard(candidate) != networkIDShard(key) {
			return candidate
		}
	}
}

func retainID(t *testing.T, c client.Client, key string, id uint32) {
	t.Helper()
	data, _ := json.Marshal(map[string]uint32{key: id})
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "global-vpc-system", Name: networkIDClaimPrefix + networkIDShard(key), Labels: map[string]string{networkIDAllocatorLabel: "network-id-claims"}}, Data: map[string]string{"claims.json": string(data)}}
	if err := c.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkIDRegistryDamageFencesExistingAndNewClaims(t *testing.T) {
	for _, damage := range []string{"rewound", "missing-sequence", "replaced-sequence", "missing-anchor", "mutable-anchor", "duplicate-unrelated", "malformed-unrelated"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			h := newAuthorityHarness(t, publicVPC("red"))
			key := "vpc/retained"
			if _, err := h.r.networkIDs(ctx, []string{key}); err != nil {
				t.Fatal(err)
			}
			seq := idConfigMap(t, h.c, networkIDSequenceName)
			anchor := idConfigMap(t, h.c, networkIDAnchorName)
			switch damage {
			case "rewound":
				seq.Data["next"] = "1"
				if err := h.c.Update(ctx, seq); err != nil {
					t.Fatal(err)
				}
			case "missing-sequence", "replaced-sequence":
				if err := h.c.Delete(ctx, seq); err != nil {
					t.Fatal(err)
				}
				if damage == "replaced-sequence" {
					seq.UID, seq.ResourceVersion = "replacement", ""
					if err := h.c.Create(ctx, seq); err != nil {
						t.Fatal(err)
					}
				}
			case "missing-anchor":
				if err := h.c.Delete(ctx, anchor); err != nil {
					t.Fatal(err)
				}
			case "mutable-anchor":
				if err := h.c.Delete(ctx, anchor); err != nil {
					t.Fatal(err)
				}
				anchor.UID, anchor.ResourceVersion, anchor.Immutable = "replacement-anchor", "", nil
				if err := h.c.Create(ctx, anchor); err != nil {
					t.Fatal(err)
				}
			case "duplicate-unrelated":
				retainID(t, h.c, otherShardKey(key), 1)
			case "malformed-unrelated":
				other := otherShardKey(key)
				retainID(t, h.c, other, 2)
				cm := idConfigMap(t, h.c, networkIDClaimPrefix+networkIDShard(other))
				cm.Data["claims.json"] = "null"
				if err := h.c.Update(ctx, cm); err != nil {
					t.Fatal(err)
				}
			}
			for _, request := range []string{key, "new/reservation"} {
				if got, err := h.r.networkIDs(ctx, []string{request}); err == nil {
					t.Fatalf("damaged registry served %q: %v", request, got)
				}
			}
			if damage == "missing-sequence" {
				if err := h.c.Get(ctx, client.ObjectKeyFromObject(seq), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
					t.Fatal("missing sequence was recreated beside retained claims")
				}
			}
		})
	}
}

func TestNetworkIDMissingSequenceDetectsUnknownFutureKey(t *testing.T) {
	h := newAuthorityHarness(t, publicVPC("red"))
	retainID(t, h.c, otherShardKey("vpc/new"), 8123)
	if _, err := h.r.networkIDs(context.Background(), []string{"vpc/new"}); err == nil {
		t.Fatal("missing sequence was bootstrapped beside an unrelated retained claim")
	}
}

func TestNetworkIDExistingClaimsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newAuthorityHarness(t, publicVPC("red"))
	got, err := h.r.networkIDs(ctx, []string{"vpc/red", "vpc/red"})
	if err != nil || len(got) != 1 || got["vpc/red"] != 1 {
		t.Fatalf("initial reservation: %v %v", got, err)
	}
	seq := idConfigMap(t, h.c, networkIDSequenceName)
	anchor := idConfigMap(t, h.c, networkIDAnchorName)
	if anchor.Immutable == nil || !*anchor.Immutable || anchor.Data["sequenceUID"] != string(seq.UID) {
		t.Fatal("reservation lacks immutable sequence identity")
	}
	if _, err = h.r.networkIDs(ctx, []string{"vpc/red"}); err != nil {
		t.Fatal(err)
	}
	if idConfigMap(t, h.c, networkIDSequenceName).ResourceVersion != seq.ResourceVersion {
		t.Fatal("existing claim consumed a new sequence reservation")
	}
}

// Inject a committed competing reservation while the caller scans claims. The
// subsequent sequence read must see it instead of diagnosing a false rollback.
type concurrentClaimScan struct {
	client.Client
	reserve func() error
}

func (c *concurrentClaimScan) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.ConfigMapList); ok && c.reserve != nil {
		f := c.reserve
		c.reserve = nil
		if err := f(); err != nil {
			return err
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func TestNetworkIDReadsSequenceAfterConcurrentClaimScan(t *testing.T) {
	ctx := context.Background()
	h := newAuthorityHarness(t, publicVPC("red"))
	if _, err := h.r.networkIDs(ctx, []string{"vpc/first"}); err != nil {
		t.Fatal(err)
	}
	concurrent := *h.r
	h.r.Client = &concurrentClaimScan{Client: h.c, reserve: func() error {
		_, err := concurrent.networkIDs(ctx, []string{"vpc/concurrent"})
		return err
	}}
	got, err := h.r.networkIDs(ctx, []string{"vpc/last"})
	if err != nil || got["vpc/last"] != 3 {
		t.Fatalf("valid concurrent reservation rejected or reused: %v %v", got, err)
	}
}

type lostNetworkIDResponse struct {
	client.Client
	name   string
	create bool
	fired  bool
}

func (c *lostNetworkIDResponse) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	err := c.Client.Create(ctx, obj, opts...)
	if err == nil && c.create && !c.fired && obj.GetName() == c.name {
		c.fired = true
		return errors.New("simulated lost committed create response")
	}
	return err
}

func (c *lostNetworkIDResponse) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	err := c.Client.Update(ctx, obj, opts...)
	if err == nil && !c.create && !c.fired && obj.GetName() == c.name {
		c.fired = true
		return errors.New("simulated lost committed update response")
	}
	return err
}

func TestNetworkIDRecoversCommittedLostResponses(t *testing.T) {
	for _, operation := range []string{"create-sequence", "create-anchor", "reserve-sequence", "publish-claim"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			h := newAuthorityHarness(t, publicVPC("red"))
			key := "vpc/retry"
			name := networkIDSequenceName
			if operation == "create-anchor" {
				name = networkIDAnchorName
			} else if operation == "publish-claim" {
				name = networkIDClaimPrefix + networkIDShard(key)
			}
			failure := &lostNetworkIDResponse{Client: h.c, name: name, create: strings.HasPrefix(operation, "create-")}
			h.r.Client = failure
			if _, err := h.r.networkIDs(ctx, []string{key}); err == nil || !failure.fired {
				t.Fatal("did not exercise committed lost response")
			}
			got, err := h.r.networkIDs(ctx, []string{key})
			want := uint32(1)
			if operation == "reserve-sequence" {
				want = 2 // The uncertain reservation is permanently burned.
			}
			if err != nil || got[key] != want {
				t.Fatalf("unsafe response recovery: %v %v; want %d", got, err, want)
			}
		})
	}
}
