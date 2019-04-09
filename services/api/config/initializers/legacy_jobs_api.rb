# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

# Config must be done before we  files; otherwise they
# won't be able to use Rails.configuration.* to initialize their
# classes.

require 'enable_jobs_api'

Server::Application.configure do
  begin
    if ActiveRecord::Base.connection.tables.include?('jobs')
      check_enable_legacy_jobs_api
    end
  rescue ActiveRecord::NoDatabaseError
    # Database hasn't been created yet
  end
end
