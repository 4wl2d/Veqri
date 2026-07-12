package com.veqri.android.data.room;

import androidx.annotation.NonNull;
import androidx.room.ColumnInfo;
import androidx.room.Entity;
import androidx.room.Index;
import androidx.room.PrimaryKey;

@Entity(
    tableName = "tasks",
    indices = {
        @Index("conversation_id"),
        @Index("created_at_epoch_millis"),
        @Index("updated_at_epoch_millis")
    }
)
public final class TaskEntity {
    @PrimaryKey
    @NonNull
    public String id = "";

    @ColumnInfo(name = "root_task_id")
    @NonNull
    public String rootTaskId = "";

    @ColumnInfo(name = "conversation_id")
    @NonNull
    public String conversationId = "";

    @NonNull
    public String goal = "";

    @ColumnInfo(name = "assigned_agent")
    @NonNull
    public String assignedAgent = "";

    @NonNull
    public String status = "CREATED";

    @ColumnInfo(name = "progress_percent")
    public int progressPercent;

    @NonNull
    public String summary = "";

    @ColumnInfo(name = "created_at_epoch_millis")
    public long createdAtEpochMillis;

    @ColumnInfo(name = "updated_at_epoch_millis")
    public long updatedAtEpochMillis;

    @ColumnInfo(name = "correlation_id")
    @NonNull
    public String correlationId = "";
}
