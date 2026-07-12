package com.veqri.android.data.room;

import androidx.annotation.NonNull;
import androidx.room.ColumnInfo;
import androidx.room.Entity;
import androidx.room.Index;
import androidx.room.PrimaryKey;

@Entity(
    tableName = "conversation_messages",
    indices = {@Index("conversation_id"), @Index("created_at_epoch_millis")}
)
public final class MessageEntity {
    @PrimaryKey
    @NonNull
    public String id = "";

    @ColumnInfo(name = "conversation_id")
    @NonNull
    public String conversationId = "";

    @NonNull
    public String author = "SYSTEM";

    @NonNull
    public String text = "";

    @ColumnInfo(name = "created_at_epoch_millis")
    public long createdAtEpochMillis;

    @ColumnInfo(name = "correlation_id")
    @NonNull
    public String correlationId = "";
}
