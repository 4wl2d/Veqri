import com.google.protobuf.gradle.GenerateProtoTask
import com.google.protobuf.gradle.proto
import org.gradle.api.DefaultTask
import org.gradle.api.GradleException
import org.gradle.api.file.DirectoryProperty
import org.gradle.api.tasks.InputDirectory
import org.gradle.api.tasks.PathSensitive
import org.gradle.api.tasks.PathSensitivity
import org.gradle.api.tasks.Sync
import org.gradle.api.tasks.TaskAction
import org.gradle.api.tasks.compile.JavaCompile
import org.gradle.work.DisableCachingByDefault
import java.io.File

plugins {
    `java-library`
    id("com.google.protobuf")
}

java {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
}

tasks.withType<JavaCompile>().configureEach {
    options.release = 17
}

val canonicalProtoDirectory = rootProject.layout.projectDirectory.dir("../../protocol/proto")
val generatedJavaDirectory = layout.buildDirectory.dir("generated/sources/proto/main/java")
val checkedInJavaDirectory = rootProject.layout.projectDirectory.dir("../../protocol/generated/android")

sourceSets {
    main {
        proto {
            srcDir(canonicalProtoDirectory)
            setIncludes(setOf("veqri/v1/device.proto"))
        }
    }
}

protobuf {
    protoc {
        artifact = "com.google.protobuf:protoc:4.35.1"
    }
    generateProtoTasks {
        ofSourceSet("main").configureEach {
            builtins {
                named("java") {
                    option("lite")
                }
            }
        }
    }
}

dependencies {
    api("com.google.protobuf:protobuf-javalite:4.35.1")
}

val protoGenerationTasks = tasks.withType<GenerateProtoTask>()

tasks.register<Sync>("regenerateAndroidProtocolBindings") {
    group = "code generation"
    description = "Regenerates the checked-in Android Java Lite protocol bindings."
    dependsOn(protoGenerationTasks)
    from(generatedJavaDirectory)
    into(checkedInJavaDirectory)
}

@DisableCachingByDefault(because = "The task verifies two source trees and has no output.")
abstract class CheckGeneratedBindings : DefaultTask() {
    @get:InputDirectory
    @get:PathSensitive(PathSensitivity.RELATIVE)
    abstract val generatedDirectory: DirectoryProperty

    @get:InputDirectory
    @get:PathSensitive(PathSensitivity.RELATIVE)
    abstract val checkedInDirectory: DirectoryProperty

    @TaskAction
    fun verify() {
        val generatedRoot = generatedDirectory.get().asFile
        val checkedInRoot = checkedInDirectory.get().asFile
        val generated = filesByRelativePath(generatedRoot)
        val checkedIn = filesByRelativePath(checkedInRoot)

        val missing = generated.keys - checkedIn.keys
        val stale = checkedIn.keys - generated.keys
        val changed = (generated.keys intersect checkedIn.keys).filter { path ->
            !generated.getValue(path).readBytes().contentEquals(checkedIn.getValue(path).readBytes())
        }

        if (missing.isNotEmpty() || stale.isNotEmpty() || changed.isNotEmpty()) {
            throw GradleException(
                buildString {
                    appendLine("Checked-in Android protocol bindings are stale.")
                    appendDifferences("missing", missing)
                    appendDifferences("stale", stale)
                    appendDifferences("changed", changed)
                    append("Run :protocol:regenerateAndroidProtocolBindings and commit the result.")
                },
            )
        }
    }

    private fun filesByRelativePath(root: File): Map<String, File> =
        root.walkTopDown()
            .filter { it.isFile }
            .associateBy { it.relativeTo(root).invariantSeparatorsPath }

    private fun StringBuilder.appendDifferences(label: String, paths: Collection<String>) {
        paths.sorted().forEach { path -> appendLine("  $label: $path") }
    }
}

val checkAndroidProtocolBindings = tasks.register<CheckGeneratedBindings>("checkAndroidProtocolBindings") {
    group = "verification"
    description = "Checks that committed Android Java Lite bindings match protoc output byte for byte."
    dependsOn(protoGenerationTasks)
    generatedDirectory.set(generatedJavaDirectory)
    checkedInDirectory.set(checkedInJavaDirectory)
}

tasks.named("check") {
    dependsOn(checkAndroidProtocolBindings)
}
