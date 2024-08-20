// Code generated by mockery v2.44.2. DO NOT EDIT.

package componentmocks

import (
	blockindexer "github.com/kaleido-io/paladin/kata/pkg/blockindexer"

	mock "github.com/stretchr/testify/mock"

	plugins "github.com/kaleido-io/paladin/kata/internal/plugins"
)

// PostInitComponents is an autogenerated mock type for the PostInitComponents type
type PostInitComponents struct {
	mock.Mock
}

// BlockIndexer provides a mock function with given fields:
func (_m *PostInitComponents) BlockIndexer() blockindexer.BlockIndexer {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for BlockIndexer")
	}

	var r0 blockindexer.BlockIndexer
	if rf, ok := ret.Get(0).(func() blockindexer.BlockIndexer); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(blockindexer.BlockIndexer)
		}
	}

	return r0
}

// PluginController provides a mock function with given fields:
func (_m *PostInitComponents) PluginController() plugins.PluginController {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for PluginController")
	}

	var r0 plugins.PluginController
	if rf, ok := ret.Get(0).(func() plugins.PluginController); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(plugins.PluginController)
		}
	}

	return r0
}

// NewPostInitComponents creates a new instance of PostInitComponents. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewPostInitComponents(t interface {
	mock.TestingT
	Cleanup(func())
}) *PostInitComponents {
	mock := &PostInitComponents{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
