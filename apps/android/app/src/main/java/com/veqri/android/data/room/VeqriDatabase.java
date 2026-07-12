package com.veqri.android.data.room;

import androidx.room.Database;
import androidx.room.RoomDatabase;

@Database(
    entities = {MessageEntity.class, TaskEntity.class},
    version = 1,
    exportSchema = true
)
public abstract class VeqriDatabase extends RoomDatabase {
    public abstract VeqriDao veqriDao();
}
