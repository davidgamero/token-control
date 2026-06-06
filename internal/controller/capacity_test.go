package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/token-control/token-control/api/v1alpha1"
)

func i64(v int64) *int64 { return &v }

func TestAddQuotaSumsFieldsAndNormalizes(t *testing.T) {
	// nil + nil => nil (no empty {} in status).
	assert.Nil(t, addQuota(nil, nil))

	a := &api.TokenQuota{TokensPerMinute: i64(100), TokensPerDay: i64(5000)}
	b := &api.TokenQuota{TokensPerMinute: i64(50), RequestsPerMinute: i64(10)}
	sum := addQuota(a, b)
	require.NotNil(t, sum)
	require.NotNil(t, sum.TokensPerMinute)
	assert.Equal(t, int64(150), *sum.TokensPerMinute, "TPM should add")
	require.NotNil(t, sum.RequestsPerMinute)
	assert.Equal(t, int64(10), *sum.RequestsPerMinute, "RPM inherited from b")
	require.NotNil(t, sum.TokensPerDay)
	assert.Equal(t, int64(5000), *sum.TokensPerDay, "day inherited from a")
	assert.Nil(t, sum.TokensPerMonth, "unset field stays nil")

	// Inputs must not be mutated by the accumulator pattern.
	assert.Equal(t, int64(100), *a.TokensPerMinute)
}

func TestAvailableQuotaFloorsAtZeroAndKeepsUnsetNil(t *testing.T) {
	capacity := &api.TokenQuota{TokensPerMinute: i64(1_000_000), RequestsPerMinute: i64(1000)}
	allocated := &api.TokenQuota{TokensPerMinute: i64(800_000), RequestsPerMinute: i64(1200)}

	avail := availableQuota(capacity, allocated)
	require.NotNil(t, avail)
	require.NotNil(t, avail.TokensPerMinute)
	assert.Equal(t, int64(200_000), *avail.TokensPerMinute, "capacity - allocated")
	require.NotNil(t, avail.RequestsPerMinute)
	assert.Equal(t, int64(0), *avail.RequestsPerMinute, "floored at zero when over")
	assert.Nil(t, avail.TokensPerDay, "no capacity declared for day => nil")
}

func TestOversubscribed(t *testing.T) {
	capacity := &api.TokenQuota{TokensPerMinute: i64(1_000_000)}

	assert.False(t, oversubscribed(capacity, &api.TokenQuota{TokensPerMinute: i64(900_000)}),
		"within capacity")
	assert.True(t, oversubscribed(capacity, &api.TokenQuota{TokensPerMinute: i64(1_000_001)}),
		"exceeds capacity")
	assert.False(t, oversubscribed(capacity, nil), "no allocation => not oversubscribed")
	assert.False(t, oversubscribed(nil, &api.TokenQuota{TokensPerMinute: i64(5)}),
		"no capacity => not oversubscribed")
	// A field with no declared capacity never trips oversubscription.
	assert.False(t, oversubscribed(capacity, &api.TokenQuota{TokensPerDay: i64(9_999_999)}),
		"allocation on an undeclared-capacity window is ignored")
}
