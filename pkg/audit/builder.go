package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apiserver/pkg/apis/audit/v1"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"
)

const (
	truncationAnnotationPrefix = "audit-pass-through.configbutler.io/"
)

// Input carries the request and response details needed to build one
// ResponseComplete audit event.
type Input struct {
	Request               *http.Request
	RequestInfo           *requestinfo.RequestInfo
	User                  authnv1.UserInfo
	RequestBody           []byte
	RequestBodyBytes      int64
	RequestBodyTruncated  bool
	ResponseBody          []byte
	ResponseBodyBytes     int64
	ResponseBodyTruncated bool
	ResponseStatusCode    int
	RequestReceivedAt     time.Time
	ResponseCompletedAt   time.Time
}

// Builder constructs Kubernetes audit events from proxied HTTP traffic.
type Builder struct {
	maxBodyBytes int64
}

// NewBuilder creates a Builder that captures at most maxBodyBytes into the
// requestObject and responseObject fields.
func NewBuilder(maxBodyBytes int64) *Builder {
	return &Builder{maxBodyBytes: maxBodyBytes}
}

// Build returns a single audit.k8s.io/v1 ResponseComplete event.
func (b *Builder) Build(input Input) (*v1.Event, error) {
	if input.Request == nil {
		return nil, fmt.Errorf("request is required")
	}
	if input.RequestInfo == nil {
		return nil, fmt.Errorf("request info is required")
	}

	requestMeta := decodeObjectMeta(input.RequestBody)
	responseMeta := decodeObjectMeta(input.ResponseBody)

	event := &v1.Event{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "audit.k8s.io/v1",
			Kind:       "Event",
		},
		Level:                    v1.LevelRequestResponse,
		AuditID:                  b.auditIDFromRequest(input.Request),
		Stage:                    v1.StageResponseComplete,
		RequestURI:               input.Request.URL.RequestURI(),
		Verb:                     input.RequestInfo.Verb,
		User:                     input.User,
		UserAgent:                input.Request.UserAgent(),
		SourceIPs:                collectSourceIPs(input.Request),
		ResponseStatus:           buildResponseStatus(input.ResponseStatusCode, input.ResponseBody),
		RequestReceivedTimestamp: metav1.NewMicroTime(input.RequestReceivedAt.UTC()),
		StageTimestamp:           metav1.NewMicroTime(input.ResponseCompletedAt.UTC()),
	}

	if input.RequestInfo.IsResourceRequest {
		event.ObjectRef = buildObjectReference(input.RequestInfo, requestMeta, responseMeta)
	}

	requestObject, requestTruncated := b.buildUnknown(
		input.RequestBody,
		input.RequestBodyBytes,
		input.RequestBodyTruncated,
	)
	if requestObject != nil {
		event.RequestObject = requestObject
	}

	responseObject, responseTruncated := b.buildUnknown(
		input.ResponseBody,
		input.ResponseBodyBytes,
		input.ResponseBodyTruncated,
	)
	if responseObject != nil {
		event.ResponseObject = responseObject
	}

	annotations := map[string]string{}
	if requestTruncated {
		annotations[truncationAnnotationPrefix+"request-truncated"] = "true"
	}
	if responseTruncated {
		annotations[truncationAnnotationPrefix+"response-truncated"] = "true"
	}
	if len(annotations) > 0 {
		event.Annotations = annotations
	}

	return event, nil
}

// Wrap wraps one event in the audit EventList payload expected by webhook
// backends.
func Wrap(event v1.Event) v1.EventList {
	return v1.EventList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "audit.k8s.io/v1",
			Kind:       "EventList",
		},
		Items: []v1.Event{event},
	}
}

func (b *Builder) auditIDFromRequest(req *http.Request) types.UID {
	if value := strings.TrimSpace(req.Header.Get(v1.HeaderAuditID)); value != "" {
		return types.UID(value)
	}

	return types.UID(uuid.NewUUID())
}

func (b *Builder) buildUnknown(body []byte, originalSize int64, truncated bool) (*runtime.Unknown, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}

	if truncated {
		truncatedPayload, err := json.Marshal(map[string]any{
			"truncated":         true,
			"originalSizeBytes": originalSize,
			"capturedBytes":     len(body),
		})
		if err != nil {
			return nil, true
		}

		return &runtime.Unknown{Raw: truncatedPayload, ContentType: runtime.ContentTypeJSON}, true
	}

	if !json.Valid(body) {
		return nil, false
	}

	return &runtime.Unknown{Raw: append([]byte(nil), body...), ContentType: runtime.ContentTypeJSON}, false
}

func buildResponseStatus(code int, body []byte) *metav1.Status {
	var status metav1.Status
	if len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &status) == nil && strings.EqualFold(status.Kind, "Status") {
		if status.APIVersion == "" {
			status.APIVersion = "v1"
		}
		if status.Kind == "" {
			status.Kind = "Status"
		}
		if status.Code == 0 {
			status.Code = int32(code)
		}

		return &status
	}

	result := &metav1.Status{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Status",
		},
		Code: int32(code),
	}
	if code >= http.StatusOK && code < http.StatusBadRequest {
		result.Status = metav1.StatusSuccess
	} else {
		result.Status = metav1.StatusFailure
	}

	return result
}

type objectMeta struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
}

type metaCarrier struct {
	Metadata objectMeta `json:"metadata"`
}

func decodeObjectMeta(body []byte) objectMeta {
	var carrier metaCarrier
	if len(bytes.TrimSpace(body)) == 0 {
		return objectMeta{}
	}
	if err := json.Unmarshal(body, &carrier); err != nil {
		return objectMeta{}
	}

	return carrier.Metadata
}

func buildObjectReference(
	info *requestinfo.RequestInfo,
	requestMeta, responseMeta objectMeta,
) *v1.ObjectReference {
	ref := &v1.ObjectReference{
		APIGroup:    info.APIGroup,
		APIVersion:  info.APIVersion,
		Resource:    info.Resource,
		Subresource: info.Subresource,
		Name:        firstNonEmpty(info.Name, requestMeta.Name, responseMeta.Name),
		Namespace:   firstNonEmpty(namespaceValue(info.Namespace), requestMeta.Namespace, responseMeta.Namespace),
		UID:         types.UID(firstNonEmpty(responseMeta.UID, requestMeta.UID)),
	}
	if rv := firstNonEmpty(responseMeta.ResourceVersion, requestMeta.ResourceVersion); rv != "" {
		ref.ResourceVersion = rv
	}

	return ref
}

func namespaceValue(namespace string) string {
	if namespace == metav1.NamespaceNone {
		return ""
	}

	return namespace
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func collectSourceIPs(req *http.Request) []string {
	seen := map[string]struct{}{}
	ips := make([]string, 0, 4)

	appendIP := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		ips = append(ips, value)
	}

	for _, part := range strings.Split(req.Header.Get("X-Forwarded-For"), ",") {
		appendIP(part)
	}
	appendIP(req.Header.Get("X-Real-IP"))

	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err == nil {
		appendIP(host)
	} else {
		appendIP(req.RemoteAddr)
	}

	return ips
}
