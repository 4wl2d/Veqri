package com.veqri.android.media

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.pm.PackageManager
import android.media.AudioDeviceCallback
import android.media.AudioDeviceInfo
import android.media.AudioManager
import android.os.Build
import androidx.core.content.ContextCompat
import com.veqri.android.data.AudioRouteKind
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

interface AudioRouteController {
    val availableRoutes: StateFlow<Set<AudioRouteKind>>
    fun start()
    fun select(route: AudioRouteKind): Boolean
    fun stop()
}

class AndroidAudioRouteController(context: Context) : AudioRouteController {
    private val appContext = context.applicationContext
    private val audioManager = appContext.getSystemService(AudioManager::class.java)
    private val mutableRoutes = MutableStateFlow(defaultRoutes())
    private var callbackRegistered = false
    private val deviceCallback = object : AudioDeviceCallback() {
        override fun onAudioDevicesAdded(addedDevices: Array<out AudioDeviceInfo>) = refreshRoutes()
        override fun onAudioDevicesRemoved(removedDevices: Array<out AudioDeviceInfo>) = refreshRoutes()
    }

    override val availableRoutes: StateFlow<Set<AudioRouteKind>> = mutableRoutes.asStateFlow()

    override fun start() {
        audioManager.mode = AudioManager.MODE_IN_COMMUNICATION
        if (!callbackRegistered) {
            audioManager.registerAudioDeviceCallback(deviceCallback, null)
            callbackRegistered = true
        }
        refreshRoutes()
    }

    @Suppress("DEPRECATION")
    override fun select(route: AudioRouteKind): Boolean {
        if (route !in mutableRoutes.value) return false
        return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            selectCommunicationDevice(route)
        } else {
            when (route) {
                AudioRouteKind.SPEAKER -> {
                    audioManager.isSpeakerphoneOn = true
                    true
                }
                AudioRouteKind.EARPIECE, AudioRouteKind.WIRED_HEADSET -> {
                    audioManager.isSpeakerphoneOn = false
                    true
                }
                AudioRouteKind.BLUETOOTH -> {
                    if (!hasBluetoothPermission()) return false
                    audioManager.startBluetoothSco()
                    audioManager.isBluetoothScoOn = true
                    true
                }
            }
        }
    }

    @Suppress("DEPRECATION")
    override fun stop() {
        if (callbackRegistered) {
            audioManager.unregisterAudioDeviceCallback(deviceCallback)
            callbackRegistered = false
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            audioManager.clearCommunicationDevice()
        } else {
            audioManager.stopBluetoothSco()
            audioManager.isBluetoothScoOn = false
            audioManager.isSpeakerphoneOn = false
        }
        audioManager.mode = AudioManager.MODE_NORMAL
        mutableRoutes.value = defaultRoutes()
    }

    private fun refreshRoutes() {
        val detected = buildSet {
            addAll(defaultRoutes())
            val devices = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                if (hasBluetoothPermission()) {
                    runCatching { audioManager.availableCommunicationDevices }.getOrDefault(emptyList())
                } else {
                    audioManager.getDevices(AudioManager.GET_DEVICES_OUTPUTS).toList()
                }
            } else {
                audioManager.getDevices(AudioManager.GET_DEVICES_OUTPUTS).toList()
            }
            devices.mapNotNullTo(this) { it.toRoute() }
        }
        mutableRoutes.value = detected
    }

    @SuppressLint("MissingPermission")
    private fun selectCommunicationDevice(route: AudioRouteKind): Boolean {
        if (route == AudioRouteKind.BLUETOOTH && !hasBluetoothPermission()) return false
        val candidate = runCatching { audioManager.availableCommunicationDevices }
            .getOrDefault(emptyList())
            .firstOrNull { it.toRoute() == route }
            ?: return false
        return runCatching { audioManager.setCommunicationDevice(candidate) }.getOrDefault(false)
    }

    private fun hasBluetoothPermission(): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.S ||
            ContextCompat.checkSelfPermission(appContext, Manifest.permission.BLUETOOTH_CONNECT) ==
            PackageManager.PERMISSION_GRANTED

    private fun AudioDeviceInfo.toRoute(): AudioRouteKind? = when (type) {
        AudioDeviceInfo.TYPE_BUILTIN_EARPIECE -> AudioRouteKind.EARPIECE
        AudioDeviceInfo.TYPE_BUILTIN_SPEAKER -> AudioRouteKind.SPEAKER
        AudioDeviceInfo.TYPE_WIRED_HEADSET,
        AudioDeviceInfo.TYPE_WIRED_HEADPHONES,
        AudioDeviceInfo.TYPE_USB_HEADSET,
        -> AudioRouteKind.WIRED_HEADSET
        AudioDeviceInfo.TYPE_BLUETOOTH_SCO,
        AudioDeviceInfo.TYPE_BLUETOOTH_A2DP,
        AudioDeviceInfo.TYPE_BLE_HEADSET,
        AudioDeviceInfo.TYPE_BLE_SPEAKER,
        -> AudioRouteKind.BLUETOOTH
        else -> null
    }

    private fun defaultRoutes() = setOf(AudioRouteKind.EARPIECE, AudioRouteKind.SPEAKER)
}
