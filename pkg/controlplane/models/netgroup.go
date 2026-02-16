package models

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

// Netgroup defines an IP-based access control group.
// Netgroups are first-class API resources that can be referenced by multiple shares.
type Netgroup struct {
	ID        string           `gorm:"primaryKey;size:36" json:"id"`
	Name      string           `gorm:"uniqueIndex;not null;size:255" json:"name"`
	Members   []NetgroupMember `gorm:"foreignKey:NetgroupID" json:"members,omitempty"`
	CreatedAt time.Time        `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time        `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for Netgroup.
func (Netgroup) TableName() string {
	return "netgroups"
}

// NetgroupMember defines a member of a netgroup.
// Members can be IP addresses, CIDR ranges, or hostnames (with wildcard support).
type NetgroupMember struct {
	ID         string    `gorm:"primaryKey;size:36" json:"id"`
	NetgroupID string    `gorm:"not null;size:36;index" json:"netgroup_id"`
	Type       string    `gorm:"not null;size:20" json:"type"`   // "ip", "cidr", "hostname"
	Value      string    `gorm:"not null;size:255" json:"value"` // IP, CIDR, or hostname pattern
	CreatedAt  time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// TableName returns the table name for NetgroupMember.
func (NetgroupMember) TableName() string {
	return "netgroup_members"
}

// ValidMemberTypes lists the allowed member types.
var ValidMemberTypes = []string{"ip", "cidr", "hostname"}

// ValidateMemberType checks if a member type string is valid.
func ValidateMemberType(memberType string) bool {
	return slices.Contains(ValidMemberTypes, memberType)
}

// ValidateMemberValue validates a member value based on its type.
func ValidateMemberValue(memberType, value string) error {
	switch memberType {
	case "ip":
		if net.ParseIP(value) == nil {
			return fmt.Errorf("invalid IP address: %s", value)
		}
		return nil

	case "cidr":
		_, _, err := net.ParseCIDR(value)
		if err != nil {
			return fmt.Errorf("invalid CIDR: %s: %w", value, err)
		}
		return nil

	case "hostname":
		if value == "" {
			return fmt.Errorf("hostname must not be empty")
		}
		// Allow wildcards like *.example.com
		if strings.HasPrefix(value, "*.") {
			// Validate the domain part after the wildcard
			domain := value[2:]
			if domain == "" {
				return fmt.Errorf("wildcard hostname must have a domain: %s", value)
			}
			return nil
		}
		// Basic hostname validation: non-empty, no spaces
		if strings.ContainsAny(value, " \t\n\r") {
			return fmt.Errorf("hostname must not contain whitespace: %s", value)
		}
		return nil

	default:
		return fmt.Errorf("unknown member type: %s", memberType)
	}
}
