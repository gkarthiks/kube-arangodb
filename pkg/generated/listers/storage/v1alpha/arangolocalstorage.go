//
// DISCLAIMER
//
// Copyright 2018 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//

// This file was automatically generated by lister-gen

package v1alpha

import (
	v1alpha "github.com/arangodb/kube-arangodb/pkg/apis/storage/v1alpha"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// ArangoLocalStorageLister helps list ArangoLocalStorages.
type ArangoLocalStorageLister interface {
	// List lists all ArangoLocalStorages in the indexer.
	List(selector labels.Selector) (ret []*v1alpha.ArangoLocalStorage, err error)
	// Get retrieves the ArangoLocalStorage from the index for a given name.
	Get(name string) (*v1alpha.ArangoLocalStorage, error)
	ArangoLocalStorageListerExpansion
}

// arangoLocalStorageLister implements the ArangoLocalStorageLister interface.
type arangoLocalStorageLister struct {
	indexer cache.Indexer
}

// NewArangoLocalStorageLister returns a new ArangoLocalStorageLister.
func NewArangoLocalStorageLister(indexer cache.Indexer) ArangoLocalStorageLister {
	return &arangoLocalStorageLister{indexer: indexer}
}

// List lists all ArangoLocalStorages in the indexer.
func (s *arangoLocalStorageLister) List(selector labels.Selector) (ret []*v1alpha.ArangoLocalStorage, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1alpha.ArangoLocalStorage))
	})
	return ret, err
}

// Get retrieves the ArangoLocalStorage from the index for a given name.
func (s *arangoLocalStorageLister) Get(name string) (*v1alpha.ArangoLocalStorage, error) {
	obj, exists, err := s.indexer.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(v1alpha.Resource("arangolocalstorage"), name)
	}
	return obj.(*v1alpha.ArangoLocalStorage), nil
}
