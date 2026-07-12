package com.veqri.android

import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.v2.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import com.veqri.android.ui.PairingScreen
import com.veqri.android.ui.VeqriAction
import com.veqri.android.ui.VeqriApp
import com.veqri.android.ui.VeqriUiState
import com.veqri.android.ui.theme.VeqriTheme
import org.junit.Rule
import org.junit.Test
import org.junit.Assert.assertEquals

class PairingScreenTest {
    @get:Rule
    val composeRule = createComposeRule()

    @Test
    fun pairingFormExplainsLocalSimulator() {
        composeRule.setContent {
            VeqriTheme {
                PairingScreen(
                    state = VeqriUiState(isPaired = false, isLocalSimulator = true),
                    onAction = {},
                )
            }
        }

        composeRule.onNodeWithTag("pairing-title").assertIsDisplayed()
        composeRule.onNodeWithTag("pairing-code").assertIsDisplayed()
        composeRule.onNodeWithText("Debug simulator code: 123456. It never contacts a real Core.")
            .assertIsDisplayed()
    }

    @Test
    fun pairedDeviceOffersManualCredentialRotation() {
        val actions = mutableListOf<VeqriAction>()
        composeRule.setContent {
            VeqriTheme {
                VeqriApp(
                    state = VeqriUiState(isPaired = true),
                    onAction = actions::add,
                )
            }
        }

        composeRule.onNodeWithTag("rotate-credential").assertIsDisplayed().performClick()
        assertEquals(VeqriAction.RotateCredential, actions.single())
    }
}
