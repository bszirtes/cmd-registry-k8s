// Copyright (c) 2020 Cisco and/or its affiliates.
//
// Copyright (c) 2021 Doc.ai and/or its affiliates.
//
// Copyright (c) 2022-2025 Cisco and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package imports is used for generating list of imports to optimize use of docker build cache
package imports

//go:generate bash -c "rm -rf imports*.go"
//go:generate bash -c "cd $(mktemp -d) && GO111MODULE=on go install github.com/edwarnicke/imports-gen@v1.1.2"
//go:generate bash -c "GOOS=linux ${GOPATH}/bin/imports-gen"
