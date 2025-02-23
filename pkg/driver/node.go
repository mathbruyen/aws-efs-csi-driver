/*
Copyright 2019 The Kubernetes Authors.

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

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog"
)

const (
	jwtTokenFilePath  = "/home/token"
	roleAnnotationKey = "eks.amazonaws.com/role-arn"
	docURL            = "https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html"
)

type podAuth struct {
	nameSpace, serviceAccount string
	k8sClient                 k8sv1.CoreV1Interface
	ctx                       context.Context
	jwtToken                  string
}

var (
	volumeCapAccessModes = []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	}
	volumeIdCounter  = make(map[string]int)
	supportedFSTypes = []string{"efs", ""}
)

func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func newPodAuth(ctx context.Context, nameSpace, serviceAccount string, k8sClient k8sv1.CoreV1Interface, JWT string) (podauth *podAuth) {
	return &podAuth{
		nameSpace:      nameSpace,
		serviceAccount: serviceAccount,
		k8sClient:      k8sClient,
		ctx:            ctx,
		jwtToken:       JWT,
	}
}

func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.V(4).Infof("NodePublishVolume: called with args %+v", req)
	mountOptions := []string{}

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}

	if err := d.isValidVolumeCapabilities([]*csi.VolumeCapability{volCap}); err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Volume capability not supported: %s", err))
	}

	if volCap.GetMount() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability access type must be mount")
	}

	// TODO when CreateVolume is implemented, it must use the same key names
	subpath := "/"
	encryptInTransit := true
	iamAuth := false
	volContext := req.GetVolumeContext()

	for k, v := range volContext {
		switch strings.ToLower(k) {
		case "csi.storage.k8s.io/pod.name", "csi.storage.k8s.io/pod.namespace", "csi.storage.k8s.io/pod.uid", "csi.storage.k8s.io/serviceaccount.name", "csi.storage.k8s.io/ephemeral":
			continue

		case "csi.storage.k8s.io/serviceaccount.tokens":
			//TODO get the token in a cleaner way, not like this.
			result := make(map[string]interface{})
			err := json.Unmarshal([]byte(volContext["csi.storage.k8s.io/serviceAccount.tokens"]), &result)
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Invalid JSON string for tokens : %s, error %v", v, err))
			}

			//Get JWT token with aud as sts.amazonaws.com
			tokenInfo := fmt.Sprintf("%v", result["sts.amazonaws.com"].(map[string]interface{})["token"])

			// Set the podauth object then call set pod credentials (ROLE-ARN and Toke file) that are called through efs-utils
			podauth := newPodAuth(ctx, volContext["csi.storage.k8s.io/pod.namespace"], volContext["csi.storage.k8s.io/serviceAccount.name"], d.k8sClient, tokenInfo)
			err = podauth.setPodCredentials()
			if err != nil {
				return nil, status.Error(codes.Internal, fmt.Sprintf("Unable to set pod iam credentials %v", err))
			}

		case "podiamauthorization":
			var err error
			iamAuth, err = strconv.ParseBool(v)
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Volume context property %q must be a boolean value: %v", k, err))
			}
		//Deprecated
		case "path":
			klog.Warning("Use of path under volumeAttributes is deprecated. This field will be removed in future release")
			if !filepath.IsAbs(v) {
				return nil, status.Errorf(codes.InvalidArgument, "Volume context property %q must be an absolute path", k)
			}
			subpath = filepath.Join(subpath, v)
		case "storage.kubernetes.io/csiprovisioneridentity":
			continue
		case "encryptintransit":
			var err error
			encryptInTransit, err = strconv.ParseBool(v)
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Volume context property %q must be a boolean value: %v", k, err))
			}
		case MountTargetIp:
			ipAddr := volContext[MountTargetIp]
			mountOptions = append(mountOptions, MountTargetIp+"="+ipAddr)
		default:
			return nil, status.Errorf(codes.InvalidArgument, "Volume context property %s not supported", k)
		}
	}

	fsid, vpath, apid, err := parseVolumeId(req.GetVolumeId())
	if err != nil {
		// parseVolumeId returns the appropriate error
		return nil, err
	}
	// The `vpath` takes precedence if specified. If not specified, we'll either use the
	// (deprecated) `path` from the volContext, or default to "/" from above.
	if vpath != "" {
		subpath = vpath
	}
	source := fmt.Sprintf("%s:%s", fsid, subpath)

	// If an access point was specified, we need to include two things in the mountOptions:
	// - The access point ID, properly prefixed. (Below, we'll check whether an access point was
	//   also specified in the incoming mount options and react appropriately.)
	// - The TLS option. Access point mounts won't work without it. (For ease of use, we won't
	//   require this to be present in the mountOptions already, but we won't complain if it is.)
	if apid != "" {
		mountOptions = append(mountOptions, fmt.Sprintf("accesspoint=%s", apid), "tls")
	}

	//Setting the mount option to iam for efs-utils to mount with IAM
	if iamAuth {
		mountOptions = append(mountOptions, "iam")
	}

	if encryptInTransit {
		// The TLS option may have been added above if apid was set
		// TODO: mountOptions should be a set to avoid all this hasOption checking
		if !hasOption(mountOptions, "tls") {
			mountOptions = append(mountOptions, "tls")
		}
	}

	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	if m := volCap.GetMount(); m != nil {
		for _, f := range m.MountFlags {
			// Special-case check for access point
			// Not sure if `accesspoint` is allowed to have mixed case, but this shouldn't hurt,
			// and it simplifies both matches (HasPrefix, hasOption) below.
			f = strings.ToLower(f)
			if strings.HasPrefix(f, "accesspoint=") {
				// The MountOptions Access Point ID
				moapid := f[12:]
				// No matter what, warn that this is not the right way to specify an access point
				klog.Warning(fmt.Sprintf(
					"Use of 'accesspoint' under mountOptions is deprecated with this driver. "+
						"Specify the access point in the volumeHandle instead, e.g. 'volumeHandle: %s:%s:%s'",
					fsid, subpath, moapid))
				// If they specified the same access point in both places, let it slide; otherwise, fail.
				if apid != "" && moapid != apid {
					return nil, status.Errorf(codes.InvalidArgument,
						"Found conflicting access point IDs in mountOptions (%s) and volumeHandle (%s)", moapid, apid)
				}
				// Fall through; the code below will uniq for us.
			}

			if f == "tls" {
				klog.Warning(
					"Use of 'tls' under mountOptions is deprecated with this driver since tls is enabled by default. " +
						"To disable it, set encrypt in transit in the volumeContext, e.g. 'encryptInTransit: true'")
				// If they set tls and encryptInTransit is true, let it slide; otherwise, fail.
				if !encryptInTransit {
					return nil, status.Errorf(codes.InvalidArgument,
						"Found tls in mountOptions but encryptInTransit is false")
				}
			}

			if strings.HasPrefix(f, "awscredsuri") {
				klog.Warning("awscredsuri mount option is not supported by efs-csi-driver.")
				continue
			}

			if !hasOption(mountOptions, f) {
				mountOptions = append(mountOptions, f)
			}
		}
	}
	klog.V(5).Infof("NodePublishVolume: creating dir %s", target)
	if err := d.mounter.MakeDir(target); err != nil {
		return nil, status.Errorf(codes.Internal, "Could not create dir %q: %v", target, err)
	}

	klog.V(5).Infof("NodePublishVolume: mounting %s at %s with options %v", source, target, mountOptions)

	if err := d.mounter.Mount(source, target, "efs", mountOptions); err != nil {
		os.Remove(target)
		return nil, status.Errorf(codes.Internal, "Could not mount %q at %q: %v", source, target, err)
	}
	klog.V(5).Infof("NodePublishVolume: %s was mounted", target)

	//Increment volume Id counter
	if d.volMetricsOptIn {
		if value, ok := volumeIdCounter[req.GetVolumeId()]; ok {
			volumeIdCounter[req.GetVolumeId()] = value + 1
		} else {
			volumeIdCounter[req.GetVolumeId()] = 1
		}
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(4).Infof("NodeUnpublishVolume: called with args %+v", req)

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	// Check if target directory is a mount point. GetDeviceNameFromMount
	// given a mnt point, finds the device from /proc/mounts
	// returns the device name, reference count, and error code
	_, refCount, err := d.mounter.GetDeviceName(target)
	if err != nil {
		format := "failed to check if volume is mounted: %v"
		return nil, status.Errorf(codes.Internal, format, err)
	}

	// From the spec: If the volume corresponding to the volume_id
	// is not staged to the staging_target_path, the Plugin MUST
	// reply 0 OK.
	if refCount == 0 {
		klog.V(5).Infof("NodeUnpublishVolume: %s target not mounted", target)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	klog.V(5).Infof("NodeUnpublishVolume: unmounting %s", target)
	err = d.mounter.Unmount(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount %q: %v", target, err)
	}
	klog.V(5).Infof("NodeUnpublishVolume: %s unmounted", target)

	//TODO: If `du` is running on a volume, unmount waits for it to complete. We should stop `du` on unmount in the future for NodeUnpublish
	//Decrement Volume ID counter and evict cache if counter is 0.
	if d.volMetricsOptIn {
		if value, ok := volumeIdCounter[req.GetVolumeId()]; ok {
			value -= 1
			if value < 1 {
				klog.V(4).Infof("Evicting vol ID: %v, vol path : %v from cache", req.VolumeId, target)
				d.volStatter.removeFromCache(req.VolumeId)
				delete(volumeIdCounter, req.GetVolumeId())
			} else {
				volumeIdCounter[req.GetVolumeId()] = value
			}
		}
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	klog.V(4).Infof("NodeGetVolumeStats: called with args %+v", req)

	volId := req.GetVolumeId()
	if volId == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	target := req.GetVolumePath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume Path not provided")
	}

	_, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "Volume Path %s does not exist", target)
		}

		return nil, status.Errorf(codes.Internal, "Failed to invoke stat on volume path %s: %v", target, err)
	}

	volMetrics, err := d.volStatter.computeVolumeMetrics(volId, target, d.volMetricsRefreshPeriod, d.volMetricsFsRateLimit)

	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not get metrics: %v ", err)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: volMetrics.volUsage,
	}, nil
}

func (d *Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	klog.V(4).Infof("NodeGetCapabilities: called with args %+v", req)
	var caps []*csi.NodeServiceCapability
	for _, cap := range d.nodeCaps {
		c := &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(4).Infof("NodeGetInfo: called with args %+v", req)

	return &csi.NodeGetInfoResponse{
		NodeId: d.nodeID,
	}, nil
}

func (d *Driver) isValidVolumeCapabilities(volCaps []*csi.VolumeCapability) error {
	if err := d.validateAccessMode(volCaps); err != nil {
		return err
	}

	if err := d.validateAccessType(volCaps); err != nil {
		return err
	}

	if err := d.validateFStype(volCaps); err != nil {
		return err
	}
	return nil
}

func (d *Driver) validateAccessMode(volCaps []*csi.VolumeCapability) error {
	isSupportedAccessMode := func(cap *csi.VolumeCapability) bool {
		for _, m := range volumeCapAccessModes {
			if m == cap.AccessMode.GetMode() {
				return true
			}
		}
		return false
	}

	var invalidModes []string
	for _, c := range volCaps {
		if !isSupportedAccessMode(c) {
			invalidModes = append(invalidModes, c.AccessMode.GetMode().String())
		}
	}
	if len(invalidModes) != 0 {
		return fmt.Errorf("invalid access mode: %s", strings.Join(invalidModes, ","))
	}
	return nil
}

func (d *Driver) validateAccessType(volCaps []*csi.VolumeCapability) error {
	for _, c := range volCaps {
		if c.GetMount() == nil {
			return fmt.Errorf("only filesystem volumes are supported")
		}
	}
	return nil
}

func (d *Driver) validateFStype(volCaps []*csi.VolumeCapability) error {
	isSupportedFStype := func(cap *csi.VolumeCapability) bool {
		for _, m := range supportedFSTypes {
			if m == cap.GetMount().FsType {
				return true
			}
		}
		return false
	}

	var invalidFStypes []string
	for _, c := range volCaps {
		if !isSupportedFStype(c) {
			invalidFStypes = append(invalidFStypes, c.GetMount().FsType)
		}
	}
	if len(invalidFStypes) != 0 {
		return fmt.Errorf("invalid fstype: %s", strings.Join(invalidFStypes, ","))
	}
	return nil
}

// parseVolumeId accepts a NodePublishVolumeRequest.VolumeId as a colon-delimited string of the
// form `{fileSystemID}:{mountPath}:{accessPointID}`.
//   - The `{fileSystemID}` is required, and expected to be of the form `fs-...`.
//   - The other two fields are optional -- they may be empty or omitted entirely. For example,
//     `fs-abcd1234::`, `fs-abcd1234:`, and `fs-abcd1234` are equivalent.
//   - The `{mountPath}`, if specified, is not required to be absolute.
//   - The `{accessPointID}` is expected to be of the form `fsap-...`.
//
// parseVolumeId returns the parsed values, of which `subpath` and `apid` may be empty; and an
// error, which will be a `status.Error` with `codes.InvalidArgument`, or `nil` if the `volumeId`
// was parsed successfully.
// See the following issues for some background:
// - https://github.com/kubernetes-sigs/aws-efs-csi-driver/issues/100
// - https://github.com/kubernetes-sigs/aws-efs-csi-driver/issues/167
func parseVolumeId(volumeId string) (fsid, subpath, apid string, err error) {
	// Might as well do this up front, since the FSID is required and first in the string
	if !isValidFileSystemId(volumeId) {
		err = status.Errorf(codes.InvalidArgument, "volume ID '%s' is invalid: Expected a file system ID of the form 'fs-...'", volumeId)
		return
	}

	tokens := strings.Split(volumeId, ":")
	if len(tokens) > 3 {
		err = status.Errorf(codes.InvalidArgument, "volume ID '%s' is invalid: Expected at most three fields separated by ':'", volumeId)
		return
	}

	// Okay, we know we have a FSID
	fsid = tokens[0]

	// Do we have a subpath?
	if len(tokens) >= 2 && tokens[1] != "" {
		subpath = path.Clean(tokens[1])
	}

	// Do we have an access point ID?
	if len(tokens) == 3 && tokens[2] != "" {
		apid = tokens[2]
		if !isValidAccessPointId(apid) {
			err = status.Errorf(codes.InvalidArgument, "volume ID '%s' has an invalid access point ID '%s': Expected it to be of the form 'fsap-...'", volumeId, apid)
			return
		}
	}

	return
}

func (p podAuth) setPodCredentials() (e error) {

	err := writeToken(p)
	if err != nil {
		return err
	}

	err = setRoleArn(p)
	if err != nil {
		return err
	}

	return nil
}

func writeToken(p podAuth) (e error) {
	destFile, err := os.Create(jwtTokenFilePath)
	if err != nil {
		klog.Errorf("Unable to create file at path %s", jwtTokenFilePath)
		return err
	}

	n, err := destFile.Write([]byte(p.jwtToken))
	if err != nil {
		klog.Errorf("Unable to write JWT token at %s", jwtTokenFilePath)
		return err
	}
	klog.Infof("Wrote JWT bytes of length %d", n)

	err = destFile.Chmod(0400)
	if err != nil {
		klog.Errorf("Unable to change the file mode of %s", jwtTokenFilePath)
		return err
	}

	err = destFile.Close()
	if err != nil {
		klog.Errorf("Error while closing file of %s", jwtTokenFilePath)
		return err
	}

	err = os.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", jwtTokenFilePath)
	if err != nil {
		klog.Errorf("Unable to set environment variable %s", err)
		return err
	}

	return nil
}

func setRoleArn(p podAuth) (e error) {
	response, err := p.k8sClient.ServiceAccounts(p.nameSpace).Get(p.ctx, p.serviceAccount, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Unable to get the service account description %s", err)
		return err
	}
	roleArn := response.Annotations[roleAnnotationKey]

	if len(roleArn) <= 0 {
		klog.Errorf("Need IAM role for service account %s (namespace: %s) - %s", p.serviceAccount, p.nameSpace, docURL)
		return fmt.Errorf("An IAM role must be associated with service account %s (namespace: %s)", p.serviceAccount, p.nameSpace)
	}

	err = os.Setenv("AWS_ROLE_ARN", roleArn)
	if err != nil {
		klog.Errorf("Unable to set environment variable %s", err)
		return err
	}

	return nil
}

// Check and avoid adding duplicate mount options
func hasOption(options []string, opt string) bool {
	for _, o := range options {
		if o == opt {
			return true
		}
	}
	return false
}

func isValidFileSystemId(filesystemId string) bool {
	return strings.HasPrefix(filesystemId, "fs-")
}

func isValidAccessPointId(accesspointId string) bool {
	return strings.HasPrefix(accesspointId, "fsap-")
}
