/* Copyright © 2021 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package util

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/apparentlymart/go-cidr/cidr"
	mapset "github.com/deckarep/golang-set"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/vmware-tanzu/nsx-operator/pkg/apis/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/nsx/services/common"
)

const wcpSystemResource = "vmware-system-shared-t1"
const HashLength int = 8

var (
	String    = common.String
	basicTags = []string{common.TagScopeNamespace, common.TagScopeNamespaceUID,
		common.TagScopeStaticRouteCRName, common.TagScopeStaticRouteCRUID,
		common.TagScopeSecurityPolicyCRName, common.TagScopeSecurityPolicyCRUID,
		common.TagScopeSubnetCRName, common.TagScopeSubnetCRUID,
		common.TagScopeSubnetPortCRName, common.TagScopeSubnetPortCRUID,
		common.TagScopeVPCCRName, common.TagScopeVPCCRUID,
		common.TagScopeIPPoolCRName, common.TagScopeIPPoolCRUID,
		common.TagScopeSubnetSetCRName, common.TagScopeSubnetSetCRUID}
	tagsScopeSet = sets.New[string]()
)

func init() {
	for _, tag := range basicTags {
		tagsScopeSet.Insert(tag)
	}
}

var log = logf.Log.WithName("pkg").WithName("utils")

func NormalizeLabels(matchLabels *map[string]string) *map[string]string {
	newLabels := make(map[string]string)
	for k, v := range *matchLabels {
		newLabels[NormalizeLabelKey(k)] = NormalizeName(v)
	}
	return &newLabels
}

func NormalizeLabelKey(key string) string {
	if len(key) <= common.MaxTagLength {
		return key
	}
	splitted := strings.Split(key, "/")
	key = splitted[len(splitted)-1]
	return NormalizeName(key)
}

func NormalizeName(name string) string {
	return normalizeNamebyLimit(name, common.MaxTagLength)
}

func normalizeNamebyLimit(name string, limit int) string {
	if len(name) <= limit {
		return name
	}
	hashString := Sha1(name)
	nameLength := limit - common.HashLength - 1
	newName := fmt.Sprintf("%s-%s", name[:nameLength], hashString[:common.HashLength])
	return newName
}

func NormalizeId(name string) string {
	newName := strings.ReplaceAll(name, ":", "_")
	if len(newName) <= common.MaxIdLength {
		return newName
	}
	hashString := Sha1(name)
	nameLength := common.MaxIdLength - HashLength - 1
	for strings.ContainsAny(string(newName[nameLength-1]), "-._") {
		nameLength--
	}
	newName = fmt.Sprintf("%s-%s", newName[:nameLength], hashString[:HashLength])
	return newName
}

func Sha1(data string) string {
	h := sha1.New()
	h.Write([]byte(data))
	sum := h.Sum(nil)
	return fmt.Sprintf("%x", sum)
}

func RemoveDuplicateStr(strSlice []string) []string {
	stringSet := mapset.NewSet()

	for _, d := range strSlice {
		stringSet.Add(d)
	}
	resultStr := make([]string, len(stringSet.ToSlice()))
	for i, v := range stringSet.ToSlice() {
		resultStr[i] = v.(string)
	}

	return resultStr
}

func ToUpper(obj interface{}) string {
	str := fmt.Sprintf("%s", obj)
	return strings.ToUpper(str)
}

func CalculateSubnetSize(mask int) int64 {
	size := 1 << uint(32-mask)
	return int64(size)
}

func IsSystemNamespace(c client.Client, ns string, obj *v1.Namespace) (bool, error) {
	nsObj := &v1.Namespace{}
	if obj != nil {
		nsObj = obj
	} else if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ns}, nsObj); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if isSysNs, ok := nsObj.Annotations[wcpSystemResource]; ok && strings.ToLower(isSysNs) == "true" {
		return true, nil
	}
	return false, nil
}

// CheckPodHasNamedPort checks if the pod has a named port, it filters the pod events
// we don't want give concern.
func CheckPodHasNamedPort(pod v1.Pod, reason string) bool {
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name != "" {
				log.V(1).Info(fmt.Sprintf("%s pod %s has a named port %s", reason, pod.Name, port.Name))
				return true
			}
		}
	}
	log.V(1).Info(fmt.Sprintf("%s pod %s has no named port", reason, pod.Name))
	return false
}

func Contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}

// RemoveIPPrefix remove the prefix from an IP address, e.g.
// "1.2.3.4/24" -> "1.2.3.4"
func RemoveIPPrefix(ipAddress string) (string, error) {
	ip := strings.Split(ipAddress, "/")[0]
	if net.ParseIP(ip) == nil {
		return "", errors.New("invalid IP address")
	}
	return ip, nil
}

// GetIPPrefix get the prefix from an IP address, e.g.
// "1.2.3.4/24" -> 24
func GetIPPrefix(ipAddress string) (int, error) {
	num, err := strconv.Atoi(strings.Split(ipAddress, "/")[1])
	if err != nil {
		return -1, err
	}
	return num, err
}

// GetSubnetMask get the mask for a given prefix length, e.g.
// 24 -> "255.255.255.0"
func GetSubnetMask(subnetLength int) (string, error) {
	if subnetLength < 0 || subnetLength > 32 {
		return "", errors.New("invalid subnet mask length")
	}
	// Create a 32-bit subnet mask with leading 1's and trailing 0's
	subnetBinary := uint32(0xffffffff) << (32 - subnetLength)
	// Convert the binary representation to dotted-decimal format
	subnetMask := net.IPv4(byte(subnetBinary>>24), byte(subnetBinary>>16), byte(subnetBinary>>8), byte(subnetBinary))
	return subnetMask.String(), nil
}

func CalculateIPFromCIDRs(IPAddresses []string) (int, error) {
	total := 0
	for _, addr := range IPAddresses {
		mask, err := strconv.Atoi(strings.Split(addr, "/")[1])
		if err != nil {
			return -1, err
		}
		total += int(cidr.AddressCount(&net.IPNet{
			IP:   net.ParseIP(strings.Split(addr, "/")[0]),
			Mask: net.CIDRMask(mask, 32),
		}))
	}
	return total, nil
}

func If(condition bool, trueVal, falseVal interface{}) interface{} {
	if condition {
		return trueVal
	} else {
		return falseVal
	}
}

func GetMapValues(in interface{}) []string {
	if in == nil {
		return make([]string, 0)
	}
	switch in.(type) {
	case map[string]string:
		ssMap := in.(map[string]string)
		values := make([]string, 0, len(ssMap))
		for _, v := range ssMap {
			values = append(values, v)
		}
		return values
	default:
		log.Info("Unsupported map format")
		return nil
	}
}

// the changes map contains key/value map that you want to change.
// if giving empty value for a key in changes map like: "mykey":"", that means removing this annotation from k8s resource
func UpdateK8sResourceAnnotation(client client.Client, ctx *context.Context, k8sObj client.Object, changes map[string]string) error {
	needUpdate := false
	anno := k8sObj.GetAnnotations() // here it may return a nil because ns do not have annotations.
	newAnno := If(anno == nil, map[string]string{}, anno).(map[string]string)
	for key, value := range changes {
		// if value is not none, it means this key/value need to add/update
		if value != "" {
			needUpdate = true
			newAnno[key] = value
		} else { // if value is empty, then this key/value need to be removed from map
			_, exist := newAnno[key]
			if exist {
				delete(newAnno, key)
				needUpdate = true
			} else {
				log.Info("No need to change ns annotation")
				needUpdate = false
			}
		}
	}
	// update k8s object
	k8sObj.SetAnnotations(newAnno)

	// only send update request when it is needed
	if needUpdate {
		err := client.Update(*ctx, k8sObj)
		if err != nil {
			return err
		}
	}
	return nil
}

// GenerateID generate id for nsx resource, some resources has complex index, so set it type to string
func GenerateID(res_id, prefix, suffix string, index string) string {
	var id strings.Builder
	if len(prefix) > 0 {
		id.WriteString(prefix)
		id.WriteString("_")
	}

	id.WriteString(res_id)
	if len(index) > 0 {
		id.WriteString("_")
		id.WriteString(index)

	}
	if len(suffix) > 0 {
		id.WriteString("_")
		id.WriteString(suffix)
	}
	return id.String()
}

func GenerateDisplayName(res_name, prefix, suffix, project, cluster string) string {
	var name strings.Builder
	if len(prefix) > 0 {
		name.WriteString(prefix)
		name.WriteString("-")
	}
	if len(cluster) > 0 {
		name.WriteString(cluster)
		name.WriteString("-")

	}
	name.WriteString(res_name)
	if len(project) > 0 {
		name.WriteString("-")
		name.WriteString(project)

	}

	if len(suffix) > 0 {
		name.WriteString("-")
		name.WriteString(suffix)
	}
	return name.String()
}

func GenerateTruncName(limit int, res_name, prefix, suffix, project, cluster string) string {
	adjusted_limit := limit - len(prefix) - len(suffix)
	for _, i := range []string{prefix, suffix} {
		if len(i) > 0 {
			adjusted_limit -= 1
		}
	}
	old_name := GenerateDisplayName(res_name, "", "", project, cluster)
	if len(old_name) > adjusted_limit {
		new_name := normalizeNamebyLimit(
			old_name, adjusted_limit)
		return GenerateDisplayName(new_name, prefix, suffix, "", "")
	}
	return GenerateDisplayName(res_name, prefix, suffix, project, cluster)
}

func BuildBasicTags(cluster string, obj interface{}, namespaceID types.UID) []model.Tag {
	tags := []model.Tag{
		{
			Scope: String(common.TagScopeCluster),
			Tag:   String(cluster),
		},
	}
	switch i := obj.(type) {
	case *v1alpha1.StaticRoute:
		tags = append(tags, model.Tag{Scope: String(common.TagScopeNamespace), Tag: String(i.ObjectMeta.Namespace)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeStaticRouteCRName), Tag: String(i.ObjectMeta.Name)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeStaticRouteCRUID), Tag: String(string(i.UID))})
	case *v1alpha1.SecurityPolicy:
		tags = append(tags, model.Tag{Scope: String(common.TagScopeNamespace), Tag: String(i.ObjectMeta.Namespace)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeSecurityPolicyCRName), Tag: String(i.ObjectMeta.Name)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeSecurityPolicyCRUID), Tag: String(string(i.UID))})
	case *v1alpha1.Subnet:
		tags = append(tags, model.Tag{Scope: String(common.TagScopeNamespace), Tag: String(i.ObjectMeta.Namespace)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeSubnetCRName), Tag: String(i.ObjectMeta.Name)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeSubnetCRUID), Tag: String(string(i.UID))})
	case *v1alpha1.SubnetPort:
		tags = append(tags, model.Tag{Scope: String(common.TagScopeNamespace), Tag: String(i.ObjectMeta.Namespace)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeSubnetPortCRName), Tag: String(i.ObjectMeta.Name)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeSubnetPortCRUID), Tag: String(string(i.UID))})
	case *v1alpha1.VPC:
		tags = append(tags, model.Tag{Scope: String(common.TagScopeNamespace), Tag: String(i.ObjectMeta.Namespace)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeVPCCRName), Tag: String(i.ObjectMeta.Name)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeVPCCRUID), Tag: String(string(i.UID))})
	case *v1alpha1.IPPool:
		tags = append(tags, model.Tag{Scope: String(common.TagScopeNamespace), Tag: String(i.ObjectMeta.Namespace)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeIPPoolCRName), Tag: String(i.ObjectMeta.Name)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeIPPoolCRUID), Tag: String(string(i.UID))})
	case *v1alpha1.SubnetSet:
		tags = append(tags, model.Tag{Scope: String(common.TagScopeNamespace), Tag: String(i.ObjectMeta.Namespace)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeSubnetSetCRName), Tag: String(i.ObjectMeta.Name)})
		tags = append(tags, model.Tag{Scope: String(common.TagScopeSubnetSetCRUID), Tag: String(string(i.UID))})
	default:
		log.Info("unknown obj type", "obj", obj)
	}

	if len(namespaceID) > 0 {
		tags = append(tags, model.Tag{Scope: String(common.TagScopeNamespaceUID), Tag: String(string(namespaceID))})
	}
	return tags
}

func AppendTags(basicTags, extraTags []model.Tag) []model.Tag {
	if basicTags == nil {
		log.Info("AppendTags", "basicTags", basicTags, "extra tags", extraTags)
		return nil
	}
	for _, tag := range extraTags {
		if !tagsScopeSet.Has(*tag.Scope) {
			basicTags = append(basicTags, tag)
		}
	}
	return basicTags
}

func Capitalize(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
