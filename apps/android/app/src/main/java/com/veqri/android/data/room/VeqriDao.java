package com.veqri.android.data.room;

import androidx.room.Dao;
import androidx.room.Query;
import androidx.room.Upsert;
import java.util.List;

@Dao
public interface VeqriDao {
    @Query("SELECT * FROM conversation_messages ORDER BY created_at_epoch_millis ASC")
    List<MessageEntity> loadMessages();

    @Query("SELECT * FROM tasks ORDER BY updated_at_epoch_millis DESC")
    List<TaskEntity> loadTasks();

    @Upsert
    void upsertMessage(MessageEntity message);

    @Upsert
    void upsertTask(TaskEntity task);

    @Query("DELETE FROM conversation_messages")
    void clearMessages();

    @Query("DELETE FROM conversation_messages WHERE conversation_id = :conversationId")
    void deleteConversationMessages(String conversationId);

    @Query("UPDATE tasks SET goal = '[transcript retention disabled]', summary = '' WHERE conversation_id = :conversationId")
    void redactConversationTasks(String conversationId);

    @Query("UPDATE tasks SET goal = '[transcript retention disabled]', summary = ''")
    void redactAllTasks();

    @Query("DELETE FROM tasks")
    void clearTasks();

    @Query("DELETE FROM tasks WHERE id = :taskId")
    void deleteTask(String taskId);
}
