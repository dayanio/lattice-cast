package io.lattice.cast
import android.app.Activity
import android.os.Bundle
import io.lattice.cast.databinding.ActivityMainBinding

class MainActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(ActivityMainBinding.inflate(layoutInflater).root)
    }
}
