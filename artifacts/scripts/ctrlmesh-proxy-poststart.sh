#!/usr/bin/env bash
#
# Copyright 2021 The Kruise Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

OUTPUT=/proc/1/fd/1
[ -w /proc/1/fd/1 ] || OUTPUT=/dev/null
METRICS_HEALTH_PORT=${METRICS_HEALTH_PORT:-5441}
MSG=""

# Wait for apiserver proxy ready
attempt_counter=0
max_attempts=60
until curl --output /dev/null --silent --head --fail http://127.0.0.1:"${METRICS_HEALTH_PORT}"/readyz; do
    if [ ${attempt_counter} -eq ${max_attempts} ];then
      MSG="${MSG} POST START failed, max attempts reached!"
      echo "${MSG}" | tee -a "${OUTPUT}"
      exit 0
    fi

    MSG="${MSG}."
    attempt_counter=$((attempt_counter+1))
    sleep 3
done
