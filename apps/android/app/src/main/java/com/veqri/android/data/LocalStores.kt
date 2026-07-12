package com.veqri.android.data

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.emptyPreferences
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStoreFile
import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import androidx.room.Room
import com.veqri.android.BuildConfig
import com.veqri.android.data.room.MessageEntity
import com.veqri.android.data.room.TaskEntity
import com.veqri.android.data.room.VeqriDatabase
import java.io.IOException
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.catch
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.withContext

interface DeviceCredentialStore {
    suspend fun read(): DeviceCredential?
    suspend fun save(credential: DeviceCredential)
    suspend fun readRotationCandidate(): CredentialRotationCandidate?
    suspend fun saveRotationCandidate(candidate: CredentialRotationCandidate)
    suspend fun promoteRotationCandidate(expectedKeyVersion: Int): DeviceCredential
    suspend fun clearRotationCandidate()
    suspend fun clear()
}

interface ClientPreferenceStore {
    val preferences: Flow<ClientPreferences>
    suspend fun setCoreBaseUrl(value: String)
    suspend fun setRetainTranscript(value: Boolean)
    suspend fun setPreferPushToTalk(value: Boolean)
}

data class CacheSnapshot(
    val messages: List<ConversationMessage>,
    val tasks: List<TaskRecord>,
)

interface ConversationCache {
    suspend fun load(): CacheSnapshot
    /** Atomically replaces the complete server-authoritative cache window. */
    suspend fun replaceAuthoritative(snapshot: CacheSnapshot)
    suspend fun upsert(message: ConversationMessage)
    suspend fun upsert(task: TaskRecord)
	suspend fun deleteTask(taskId: String)
	 suspend fun deleteConversation(conversationId: String)
	suspend fun clearTranscriptContent()
    suspend fun clearAll()
}

class DataStoreClientPreferenceStore(
    context: Context,
    scope: CoroutineScope,
) : ClientPreferenceStore {
    private val dataStore: DataStore<Preferences> = PreferenceDataStoreFactory.create(
        scope = scope,
        produceFile = { context.preferencesDataStoreFile(FILE_NAME) },
    )

    override val preferences: Flow<ClientPreferences> = dataStore.data
        .catch { error ->
            if (error is IOException) emit(emptyPreferences()) else throw error
        }
        .map { values ->
            ClientPreferences(
                coreBaseUrl = values[CORE_BASE_URL] ?: BuildConfig.DEFAULT_CORE_URL,
                retainTranscript = values[RETAIN_TRANSCRIPT] ?: true,
                preferPushToTalk = values[PREFER_PUSH_TO_TALK] ?: false,
            )
        }

    override suspend fun setCoreBaseUrl(value: String) {
        dataStore.edit { it[CORE_BASE_URL] = value }
    }

    override suspend fun setRetainTranscript(value: Boolean) {
        dataStore.edit { it[RETAIN_TRANSCRIPT] = value }
    }

    override suspend fun setPreferPushToTalk(value: Boolean) {
        dataStore.edit { it[PREFER_PUSH_TO_TALK] = value }
    }

    companion object {
        private const val FILE_NAME = "veqri"
        private val CORE_BASE_URL = stringPreferencesKey("core_base_url")
        private val RETAIN_TRANSCRIPT = booleanPreferencesKey("retain_transcript")
        private val PREFER_PUSH_TO_TALK = booleanPreferencesKey("prefer_push_to_talk")
    }
}

class AndroidKeystoreCredentialStore(
    context: Context,
    private val ioDispatcher: CoroutineDispatcher = Dispatchers.IO,
) : DeviceCredentialStore {
    private val preferences = context.getSharedPreferences(PREFERENCES_FILE, Context.MODE_PRIVATE)

    override suspend fun read(): DeviceCredential? = withContext(ioDispatcher) {
        val encryptedToken = preferences.getString(KEY_TOKEN, null) ?: return@withContext null
        val iv = preferences.getString(KEY_IV, null) ?: return@withContext null
        try {
            DeviceCredential(
                deviceId = preferences.getString(KEY_DEVICE_ID, null)
                    ?: throw CredentialStorageException("Stored device identity is incomplete."),
                accessToken = decryptToken(encryptedToken, iv),
                coreBaseUrl = preferences.getString(KEY_CORE_URL, null)
                    ?: throw CredentialStorageException("Stored Core endpoint is incomplete."),
                issuedAtEpochMillis = preferences.getLong(KEY_ISSUED_AT, 0),
                keyVersion = preferences.getInt(KEY_VERSION, 1).coerceAtLeast(1),
            )
        } catch (error: CredentialStorageException) {
            throw error
        } catch (error: Exception) {
            throw CredentialStorageException("The paired device credential could not be decrypted.", error)
        }
    }

    override suspend fun save(credential: DeviceCredential) = withContext(ioDispatcher) {
        try {
            val encrypted = encryptToken(credential.accessToken)
            val editor = preferences.edit()
                .putString(KEY_TOKEN, encrypted.ciphertext)
                .putString(KEY_IV, encrypted.iv)
                .putString(KEY_DEVICE_ID, credential.deviceId)
                .putString(KEY_CORE_URL, credential.coreBaseUrl)
                .putLong(KEY_ISSUED_AT, credential.issuedAtEpochMillis)
                .putInt(KEY_VERSION, credential.keyVersion.coerceAtLeast(1))
            removeCandidate(editor)
            val committed = editor.commit()
            if (!committed) throw CredentialStorageException("The paired credential was not persisted.")
        } catch (error: CredentialStorageException) {
            throw error
        } catch (error: Exception) {
            throw CredentialStorageException("The paired credential could not be encrypted.", error)
        }
    }

    override suspend fun readRotationCandidate(): CredentialRotationCandidate? = withContext(ioDispatcher) {
        val encryptedToken = preferences.getString(CANDIDATE_TOKEN, null)
        val iv = preferences.getString(CANDIDATE_IV, null)
        if (encryptedToken == null && iv == null) return@withContext null
        try {
            if (encryptedToken == null || iv == null) {
                throw CredentialStorageException("Stored credential rotation state is incomplete.")
            }
            val candidate = CredentialRotationCandidate(
                deviceId = preferences.getString(CANDIDATE_DEVICE_ID, null)
                    ?: throw CredentialStorageException("Stored credential rotation device is incomplete."),
                accessToken = decryptToken(encryptedToken, iv),
                coreBaseUrl = preferences.getString(CANDIDATE_CORE_URL, null)
                    ?: throw CredentialStorageException("Stored credential rotation endpoint is incomplete."),
                keyVersion = preferences.getInt(CANDIDATE_VERSION, 0),
                preparedAtEpochMillis = preferences.getLong(CANDIDATE_PREPARED_AT, 0),
                expiresAtEpochMillis = preferences.getLong(CANDIDATE_EXPIRES_AT, 0),
                correlationId = preferences.getString(CANDIDATE_CORRELATION_ID, null)
                    ?: throw CredentialStorageException("Stored credential rotation correlation is incomplete."),
            )
            if (candidate.keyVersion < 2 || candidate.preparedAtEpochMillis <= 0 ||
                candidate.expiresAtEpochMillis <= candidate.preparedAtEpochMillis
            ) {
                throw CredentialStorageException("Stored credential rotation metadata is invalid.")
            }
            candidate
        } catch (error: CredentialStorageException) {
            throw error
        } catch (error: Exception) {
            throw CredentialStorageException("The pending device credential could not be decrypted.", error)
        }
    }

    override suspend fun saveRotationCandidate(candidate: CredentialRotationCandidate) = withContext(ioDispatcher) {
        try {
            val activeDeviceId = preferences.getString(KEY_DEVICE_ID, null)
                ?: throw CredentialStorageException("No active device credential is available for rotation.")
            val activeCoreUrl = preferences.getString(KEY_CORE_URL, null)
                ?: throw CredentialStorageException("The active Core endpoint is unavailable.")
            val activeVersion = preferences.getInt(KEY_VERSION, 1).coerceAtLeast(1)
            if (candidate.deviceId != activeDeviceId || candidate.coreBaseUrl != activeCoreUrl ||
                candidate.keyVersion <= activeVersion
            ) {
                throw CredentialStorageException("The replacement credential does not match the active device.")
            }
            val encrypted = encryptToken(candidate.accessToken)
            val committed = preferences.edit()
                .putString(CANDIDATE_TOKEN, encrypted.ciphertext)
                .putString(CANDIDATE_IV, encrypted.iv)
                .putString(CANDIDATE_DEVICE_ID, candidate.deviceId)
                .putString(CANDIDATE_CORE_URL, candidate.coreBaseUrl)
                .putInt(CANDIDATE_VERSION, candidate.keyVersion)
                .putLong(CANDIDATE_PREPARED_AT, candidate.preparedAtEpochMillis)
                .putLong(CANDIDATE_EXPIRES_AT, candidate.expiresAtEpochMillis)
                .putString(CANDIDATE_CORRELATION_ID, candidate.correlationId)
                .commit()
            if (!committed) throw CredentialStorageException("The replacement credential was not persisted.")
        } catch (error: CredentialStorageException) {
            throw error
        } catch (error: Exception) {
            throw CredentialStorageException("The replacement credential could not be encrypted.", error)
        }
    }

    override suspend fun promoteRotationCandidate(expectedKeyVersion: Int): DeviceCredential =
        withContext(ioDispatcher) {
            try {
                val encryptedToken = preferences.getString(CANDIDATE_TOKEN, null)
                    ?: throw CredentialStorageException("No replacement credential is available to promote.")
                val iv = preferences.getString(CANDIDATE_IV, null)
                    ?: throw CredentialStorageException("The replacement credential is incomplete.")
                val keyVersion = preferences.getInt(CANDIDATE_VERSION, 0)
                if (keyVersion != expectedKeyVersion) {
                    throw CredentialStorageException("The replacement credential version changed unexpectedly.")
                }
                val promoted = DeviceCredential(
                    deviceId = preferences.getString(CANDIDATE_DEVICE_ID, null)
                        ?: throw CredentialStorageException("The replacement device identity is incomplete."),
                    accessToken = decryptToken(encryptedToken, iv),
                    coreBaseUrl = preferences.getString(CANDIDATE_CORE_URL, null)
                        ?: throw CredentialStorageException("The replacement Core endpoint is incomplete."),
                    issuedAtEpochMillis = preferences.getLong(CANDIDATE_PREPARED_AT, 0),
                    keyVersion = keyVersion,
                )
                val editor = preferences.edit()
                    .putString(KEY_TOKEN, encryptedToken)
                    .putString(KEY_IV, iv)
                    .putString(KEY_DEVICE_ID, promoted.deviceId)
                    .putString(KEY_CORE_URL, promoted.coreBaseUrl)
                    .putLong(KEY_ISSUED_AT, promoted.issuedAtEpochMillis)
                    .putInt(KEY_VERSION, promoted.keyVersion)
                removeCandidate(editor)
                if (!editor.commit()) {
                    throw CredentialStorageException("The replacement credential could not be promoted.")
                }
                promoted
            } catch (error: CredentialStorageException) {
                throw error
            } catch (error: Exception) {
                throw CredentialStorageException("The replacement credential could not be promoted.", error)
            }
        }

    override suspend fun clearRotationCandidate() = withContext(ioDispatcher) {
        val editor = preferences.edit()
        removeCandidate(editor)
        if (!editor.commit()) {
            throw CredentialStorageException("The pending replacement credential could not be cleared.")
        }
    }

    override suspend fun clear() = withContext(ioDispatcher) {
        if (!preferences.edit().clear().commit()) {
            throw CredentialStorageException("The stored device credential could not be cleared.")
        }
    }

    private fun encryptToken(accessToken: String): EncryptedToken {
        require(accessToken.isNotBlank()) { "A device credential cannot be empty." }
        val cipher = Cipher.getInstance(TRANSFORMATION)
        cipher.init(Cipher.ENCRYPT_MODE, getOrCreateKey())
        val ciphertext = cipher.doFinal(accessToken.toByteArray(Charsets.UTF_8))
        return EncryptedToken(
            ciphertext = Base64.encodeToString(ciphertext, Base64.NO_WRAP),
            iv = Base64.encodeToString(cipher.iv, Base64.NO_WRAP),
        )
    }

    private fun decryptToken(ciphertext: String, iv: String): String {
        val cipher = Cipher.getInstance(TRANSFORMATION)
        cipher.init(
            Cipher.DECRYPT_MODE,
            getOrCreateKey(),
            GCMParameterSpec(GCM_TAG_LENGTH_BITS, Base64.decode(iv, Base64.NO_WRAP)),
        )
        return String(cipher.doFinal(Base64.decode(ciphertext, Base64.NO_WRAP)), Charsets.UTF_8)
    }

    private fun removeCandidate(editor: android.content.SharedPreferences.Editor) {
        editor.remove(CANDIDATE_TOKEN)
            .remove(CANDIDATE_IV)
            .remove(CANDIDATE_DEVICE_ID)
            .remove(CANDIDATE_CORE_URL)
            .remove(CANDIDATE_VERSION)
            .remove(CANDIDATE_PREPARED_AT)
            .remove(CANDIDATE_EXPIRES_AT)
            .remove(CANDIDATE_CORRELATION_ID)
    }

    private fun getOrCreateKey(): SecretKey {
        val keyStore = KeyStore.getInstance(ANDROID_KEYSTORE).apply { load(null) }
        (keyStore.getKey(KEY_ALIAS, null) as? SecretKey)?.let { return it }
        val keyGenerator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, ANDROID_KEYSTORE)
        keyGenerator.init(
            KeyGenParameterSpec.Builder(
                KEY_ALIAS,
                KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
            )
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setRandomizedEncryptionRequired(true)
                .build(),
        )
        return keyGenerator.generateKey()
    }

    companion object {
        private const val ANDROID_KEYSTORE = "AndroidKeyStore"
        private const val KEY_ALIAS = "veqri.device.credential.v1"
        private const val TRANSFORMATION = "AES/GCM/NoPadding"
        private const val GCM_TAG_LENGTH_BITS = 128
        private const val PREFERENCES_FILE = "veqri.secure_device"
        private const val KEY_TOKEN = "access_token_ciphertext"
        private const val KEY_IV = "access_token_iv"
        private const val KEY_DEVICE_ID = "device_id"
        private const val KEY_CORE_URL = "core_url"
        private const val KEY_ISSUED_AT = "issued_at"
        private const val KEY_VERSION = "key_version"
        private const val CANDIDATE_TOKEN = "candidate_access_token_ciphertext"
        private const val CANDIDATE_IV = "candidate_access_token_iv"
        private const val CANDIDATE_DEVICE_ID = "candidate_device_id"
        private const val CANDIDATE_CORE_URL = "candidate_core_url"
        private const val CANDIDATE_VERSION = "candidate_key_version"
        private const val CANDIDATE_PREPARED_AT = "candidate_prepared_at"
        private const val CANDIDATE_EXPIRES_AT = "candidate_expires_at"
        private const val CANDIDATE_CORRELATION_ID = "candidate_correlation_id"
    }
}

private data class EncryptedToken(val ciphertext: String, val iv: String)

class RoomConversationCache(
    private val database: VeqriDatabase,
    private val ioDispatcher: CoroutineDispatcher = Dispatchers.IO,
) : ConversationCache {
    override suspend fun load(): CacheSnapshot = withContext(ioDispatcher) {
        CacheSnapshot(
            messages = database.veqriDao().loadMessages().map(MessageEntity::toDomain),
            tasks = database.veqriDao().loadTasks().map(TaskEntity::toDomain),
        )
    }

    override suspend fun replaceAuthoritative(snapshot: CacheSnapshot) = withContext(ioDispatcher) {
        database.runInTransaction {
            database.veqriDao().clearMessages()
            database.veqriDao().clearTasks()
            snapshot.messages.forEach { database.veqriDao().upsertMessage(it.toEntity()) }
            snapshot.tasks.forEach { database.veqriDao().upsertTask(it.toEntity()) }
        }
    }

    override suspend fun upsert(message: ConversationMessage) = withContext(ioDispatcher) {
        database.veqriDao().upsertMessage(message.toEntity())
    }

    override suspend fun upsert(task: TaskRecord) = withContext(ioDispatcher) {
        database.veqriDao().upsertTask(task.toEntity())
    }

	override suspend fun deleteTask(taskId: String) = withContext(ioDispatcher) {
		database.veqriDao().deleteTask(taskId)
	}

	override suspend fun deleteConversation(conversationId: String) = withContext(ioDispatcher) {
		database.runInTransaction {
			database.veqriDao().deleteConversationMessages(conversationId)
			database.veqriDao().redactConversationTasks(conversationId)
		}
	}

	override suspend fun clearTranscriptContent() = withContext(ioDispatcher) {
		database.runInTransaction {
			database.veqriDao().clearMessages()
			database.veqriDao().redactAllTasks()
		}
	}

    override suspend fun clearAll() = withContext(ioDispatcher) {
        database.runInTransaction {
            database.veqriDao().clearMessages()
            database.veqriDao().clearTasks()
        }
    }

    companion object {
        fun create(context: Context): RoomConversationCache {
            val database = Room.databaseBuilder(
                context.applicationContext,
                VeqriDatabase::class.java,
                "veqri-cache.db",
            ).build()
            return RoomConversationCache(database)
        }
    }
}

private fun ConversationMessage.toEntity() = MessageEntity().also { entity ->
    entity.id = id
    entity.conversationId = conversationId
    entity.author = author.name
    entity.text = text
    entity.createdAtEpochMillis = createdAtEpochMillis
    entity.correlationId = correlationId
}

private fun MessageEntity.toDomain() = ConversationMessage(
    id = id,
    conversationId = conversationId,
    author = runCatching { MessageAuthor.valueOf(author) }.getOrDefault(MessageAuthor.SYSTEM),
    text = text,
    createdAtEpochMillis = createdAtEpochMillis,
    correlationId = correlationId,
)

private fun TaskRecord.toEntity() = TaskEntity().also { entity ->
    entity.id = id
    entity.rootTaskId = rootTaskId
    entity.conversationId = conversationId
    entity.goal = goal
    entity.assignedAgent = assignedAgent
    entity.status = status.name
    entity.progressPercent = progressPercent
    entity.summary = summary
    entity.createdAtEpochMillis = createdAtEpochMillis
    entity.updatedAtEpochMillis = updatedAtEpochMillis
    entity.correlationId = correlationId
}

private fun TaskEntity.toDomain() = TaskRecord(
    id = id,
    rootTaskId = rootTaskId,
    conversationId = conversationId,
    goal = goal,
    assignedAgent = assignedAgent,
    status = runCatching { TaskStatus.valueOf(status) }.getOrDefault(TaskStatus.CREATED),
    progressPercent = progressPercent,
    summary = summary,
    createdAtEpochMillis = createdAtEpochMillis,
    updatedAtEpochMillis = updatedAtEpochMillis,
    correlationId = correlationId,
)

class CredentialStorageException(message: String, cause: Throwable? = null) :
    IllegalStateException(message, cause)
