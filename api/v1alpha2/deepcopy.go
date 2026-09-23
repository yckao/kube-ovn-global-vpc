package v1alpha2

import "k8s.io/apimachinery/pkg/runtime"

func copySlice[T any](in []T) []T {
	if in == nil {
		return nil
	}
	return append(make([]T, 0, len(in)), in...)
}

func (in *VPC) DeepCopyInto(out *VPC) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status.Bindings = copySlice(in.Status.Bindings)
	out.Status.SubnetClaims = copySlice(in.Status.SubnetClaims)
	out.Status.Conditions = copySlice(in.Status.Conditions)
}
func (in *VPC) DeepCopy() *VPC {
	if in == nil {
		return nil
	}
	out := new(VPC)
	in.DeepCopyInto(out)
	return out
}
func (in *VPC) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}
func (in *VPCList) DeepCopyInto(out *VPCList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]VPC, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
func (in *VPCList) DeepCopy() *VPCList {
	if in == nil {
		return nil
	}
	out := new(VPCList)
	in.DeepCopyInto(out)
	return out
}
func (in *VPCList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

func (in *Subnet) DeepCopyInto(out *Subnet) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status.Conditions = copySlice(in.Status.Conditions)
}
func (in *Subnet) DeepCopy() *Subnet {
	if in == nil {
		return nil
	}
	out := new(Subnet)
	in.DeepCopyInto(out)
	return out
}
func (in *Subnet) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}
func (in *SubnetList) DeepCopyInto(out *SubnetList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Subnet, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
func (in *SubnetList) DeepCopy() *SubnetList {
	if in == nil {
		return nil
	}
	out := new(SubnetList)
	in.DeepCopyInto(out)
	return out
}
func (in *SubnetList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

func (in *NetworkBinding) DeepCopyInto(out *NetworkBinding) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.Subnets = copySlice(in.Spec.Subnets)
	out.Spec.AuthorizedLocations = copySlice(in.Spec.AuthorizedLocations)
	out.Spec.RemoteSubnets = copySlice(in.Spec.RemoteSubnets)
	out.Spec.Peers = copySlice(in.Spec.Peers)
	out.Spec.LinkAllocations = copySlice(in.Spec.LinkAllocations)
	for i := range out.Spec.Peers {
		out.Spec.Peers[i].Endpoint.Links = copySlice(in.Spec.Peers[i].Endpoint.Links)
	}
	out.Status.Resources = copySlice(in.Status.Resources)
	out.Status.Gateways = copySlice(in.Status.Gateways)
	for i := range out.Status.Gateways {
		out.Status.Gateways[i].Links = copySlice(in.Status.Gateways[i].Links)
	}
	out.Status.Conditions = copySlice(in.Status.Conditions)
}
func (in *NetworkBinding) DeepCopy() *NetworkBinding {
	if in == nil {
		return nil
	}
	out := new(NetworkBinding)
	in.DeepCopyInto(out)
	return out
}
func (in *NetworkBinding) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}
func (in *NetworkBindingList) DeepCopyInto(out *NetworkBindingList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]NetworkBinding, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
func (in *NetworkBindingList) DeepCopy() *NetworkBindingList {
	if in == nil {
		return nil
	}
	out := new(NetworkBindingList)
	in.DeepCopyInto(out)
	return out
}
func (in *NetworkBindingList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}
