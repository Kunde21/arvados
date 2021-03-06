# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

class RecreateCollectionUniqueNameIndex < ActiveRecord::Migration[4.2]
  def up
    Collection.transaction do
      remove_index(:collections,
                   name: 'index_collections_on_owner_uuid_and_name')
      add_index(:collections, [:owner_uuid, :name],
                unique: true,
                where: 'is_trashed = false AND current_version_uuid = uuid',
                name: 'index_collections_on_owner_uuid_and_name')
    end
  end

  def down
    Collection.transaction do
      remove_index(:collections,
                   name: 'index_collections_on_owner_uuid_and_name')
      add_index(:collections, [:owner_uuid, :name],
                unique: true,
                where: 'is_trashed = false',
                name: 'index_collections_on_owner_uuid_and_name')
    end
  end
end
