package conditions

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DittoServer condition types
const (
	// ConditionReady indicates the DittoServer is fully operational
	ConditionReady = "Ready"

	// ConditionAvailable indicates the StatefulSet has minimum ready replicas
	ConditionAvailable = "Available"

	// ConditionConfigReady indicates ConfigMap and secrets are valid
	ConditionConfigReady = "ConfigReady"

	// ConditionDatabaseReady indicates PostgreSQL (Percona) is ready (when enabled)
	ConditionDatabaseReady = "DatabaseReady"

	// ConditionProgressing indicates a change is being applied
	ConditionProgressing = "Progressing"
)

// ConditionType is a constraint for types that can be used as condition types
// Any type with underlying type string can be used (e.g., type MyCondition string)
type ConditionType interface {
	~string
}

// SetCondition adds or updates a condition in the conditions slice
// If the condition already exists with a different status, it updates the LastTransitionTime
// Accepts any type with underlying type string (e.g., custom condition type enums)
func SetCondition[T ConditionType](
	conditions *[]metav1.Condition, generation int64, conditionType T,
	status metav1.ConditionStatus, reason, message string,
) {
	now := metav1.NewTime(time.Now())
	condTypeStr := string(conditionType)

	// Find existing condition
	for i := range *conditions {
		if (*conditions)[i].Type == condTypeStr {
			// Update existing condition
			if (*conditions)[i].Status != status {
				(*conditions)[i].LastTransitionTime = now
			}
			(*conditions)[i].Status = status
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = message
			(*conditions)[i].ObservedGeneration = generation
			return
		}
	}

	// Add new condition
	*conditions = append(*conditions, metav1.Condition{
		Type:               condTypeStr,
		Status:             status,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// RemoveCondition removes a condition from the conditions slice
// Accepts any type with underlying type string (e.g., custom condition type enums)
func RemoveCondition[T ConditionType](conditions *[]metav1.Condition, conditionType T) {
	condTypeStr := string(conditionType)
	for i := range *conditions {
		if (*conditions)[i].Type == condTypeStr {
			*conditions = append((*conditions)[:i], (*conditions)[i+1:]...)
			return
		}
	}
}

// GetCondition returns the condition with the specified type, or nil if not found
// Accepts any type with underlying type string (e.g., custom condition type enums)
func GetCondition[T ConditionType](conditions []metav1.Condition, conditionType T) *metav1.Condition {
	condTypeStr := string(conditionType)
	for i := range conditions {
		if conditions[i].Type == condTypeStr {
			return &conditions[i]
		}
	}
	return nil
}

// IsConditionTrue returns true if the condition exists and has status True
// Accepts any type with underlying type string (e.g., custom condition type enums)
func IsConditionTrue[T ConditionType](conditions []metav1.Condition, conditionType T) bool {
	condition := GetCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}
