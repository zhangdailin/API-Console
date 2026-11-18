package warp

import (
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func splitContinuationPath(path string) []string { return strings.Split(path, ".") }

// validateContinuationPath accepts unknown names as official no-ops, but never
// interprets an unknown prefix as permission to mutate an identity or map key.
// Known repeated messages may be traversed pairwise; maps may only be replaced
// as a whole. Append masks must terminate at a singular string field.
func validateContinuationPath(descriptor protoreflect.MessageDescriptor, path string, appendOnly bool) string {
	parts := splitContinuationPath(path)
	for _, part := range parts {
		if part == "id" || part == "task_id" {
			return "identity_mutation"
		}
		if !validContinuationPathName(part) {
			return "invalid_mask"
		}
	}
	for i, part := range parts {
		field := descriptor.Fields().ByName(protoreflect.Name(part))
		if field == nil {
			return ""
		}
		if i == len(parts)-1 {
			if appendOnly && (field.Kind() != protoreflect.StringKind || field.IsList() || field.IsMap()) {
				return "invalid_mask"
			}
			return ""
		}
		if field.IsMap() || field.Kind() != protoreflect.MessageKind {
			return "invalid_mask"
		}
		descriptor = field.Message()
	}
	return "invalid_mask"
}

func validContinuationPathName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// knownContinuationPath resolves the entire descriptor path before mutating
// anything, so an unknown leaf cannot create an empty parent or switch a oneof.
func knownContinuationPath(descriptor protoreflect.MessageDescriptor, path []string) bool {
	for i, part := range path {
		field := descriptor.Fields().ByName(protoreflect.Name(part))
		if field == nil {
			return false
		}
		if i != len(path)-1 {
			if field.IsMap() || field.Kind() != protoreflect.MessageKind {
				return false
			}
			descriptor = field.Message()
		}
	}
	return true
}

// applyContinuationField clones replacement values and uses protobuf defaults
// for absent source leaves. The caller works on a clone, so a rejected array
// shape cannot partially change the accumulator. Unknown mask fields are no-ops;
// binary unknown fields are rejected before any mask is considered.
func applyContinuationField(destination, source protoreflect.Message, path []string, appendOnly bool) error {
	if len(path) == 0 || !knownContinuationPath(source.Descriptor(), path) {
		return nil
	}
	field := source.Descriptor().Fields().ByName(protoreflect.Name(path[0]))
	if len(path) > 1 {
		if field.IsList() {
			sourceList, destinationList := source.Get(field).List(), destination.Get(field).List()
			if sourceList.Len() != destinationList.Len() {
				return errContinuationState
			}
			for i := 0; i < sourceList.Len(); i++ {
				if err := applyContinuationField(destinationList.Get(i).Message(), sourceList.Get(i).Message(), path[1:], appendOnly); err != nil {
					return err
				}
			}
			return nil
		}
		// Get returns a valid read-only empty message for an absent field;
		// Mutable materializes only the selected destination parent.
		return applyContinuationField(destination.Mutable(field).Message(), source.Get(field).Message(), path[1:], appendOnly)
	}
	if appendOnly {
		if field.Kind() != protoreflect.StringKind || field.IsList() || field.IsMap() {
			return errContinuationState
		}
		current, delta := destination.Get(field).String(), source.Get(field).String()
		if size := len(current) + len(delta); size > continuationStateLimit {
			return continuationError("state_size", 0, size)
		}
		destination.Set(field, protoreflect.ValueOfString(current+delta))
		return nil
	}
	// Clone just this field through a temporary message to copy lists, maps,
	// bytes and nested messages without retaining aliases to the stream event.
	temporary := source.New()
	if source.Has(field) {
		temporary.Set(field, source.Get(field))
	}
	temporary = proto.Clone(temporary.Interface()).ProtoReflect()
	destination.Clear(field)
	if temporary.Has(field) {
		destination.Set(field, temporary.Get(field))
	}
	return nil
}
