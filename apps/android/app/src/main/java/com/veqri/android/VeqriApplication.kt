package com.veqri.android

import android.app.Application
import com.veqri.android.call.AndroidCallLifecycleController
import com.veqri.android.data.AndroidKeystoreCredentialStore
import com.veqri.android.data.DataStoreClientPreferenceStore
import com.veqri.android.data.RoomConversationCache
import com.veqri.android.data.VeqriRepository
import com.veqri.android.media.AndroidAudioRouteController
import com.veqri.android.media.AndroidTextToSpeechPlayback
import com.veqri.android.media.SimulatedVoiceMediaTransport
import com.veqri.android.media.UnavailableVoiceMediaTransport
import com.veqri.android.network.FakeCoreTransport
import com.veqri.android.network.OkHttpCoreTransport
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob

class VeqriApplication : Application() {
    val appScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    lateinit var container: AppContainer
        private set

    override fun onCreate() {
        super.onCreate()
        container = AppContainer(this, appScope)
    }

    override fun onTerminate() {
        container.close()
        super.onTerminate()
    }
}

class AppContainer(
    application: Application,
    appScope: CoroutineScope,
) {
    private val transport = if (BuildConfig.USE_FAKE_TRANSPORT) {
        FakeCoreTransport()
    } else {
        OkHttpCoreTransport(appScope)
    }
    private val audioRoutes = AndroidAudioRouteController(application)
    private val speechPlayback = AndroidTextToSpeechPlayback(application)
    private val mediaTransport = if (BuildConfig.USE_FAKE_TRANSPORT) {
        SimulatedVoiceMediaTransport(audioRoutes)
    } else {
        UnavailableVoiceMediaTransport()
    }

    val repository = VeqriRepository(
        scope = appScope,
        transport = transport,
        credentialStore = AndroidKeystoreCredentialStore(application),
        preferenceStore = DataStoreClientPreferenceStore(application, appScope),
        cache = RoomConversationCache.create(application),
        mediaTransport = mediaTransport,
        audioRoutes = audioRoutes,
        calls = AndroidCallLifecycleController(application),
        speechPlayback = speechPlayback,
    )

    fun close() {
        speechPlayback.close()
    }
}
